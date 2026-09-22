package cloudsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrVehicleBusy = errors.New("vehicle/logger is busy")

var replicatedTables = map[string]bool{
	"vehicles": true, "states": true, "drives": true, "positions": true,
	"charging_sessions": true, "charging_samples": true,
	"battery_samples": true, "software_updates": true,
}

type Change struct {
	Sequence  int64
	TableName string
	RowID     int64
	Operation string
}

type Status struct {
	SyncState           string `json:"sync_state"`
	LastSyncStarted     string `json:"last_sync_started,omitempty"`
	LastSyncCompleted   string `json:"last_sync_completed,omitempty"`
	LastSyncError       string `json:"last_sync_error,omitempty"`
	PendingRows         int64  `json:"pending_rows"`
	ManualSyncRequested bool   `json:"manual_sync_requested"`
}

func OpenLocal(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	return db, nil
}

// Recover resets work whose cloud outcome became uncertain after a crash.
// Resending is safe because all writes use stable primary keys and UPSERT.
func Recover(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `UPDATE cloud_sync_changes SET state='pending' WHERE state='syncing'`)
	return err
}

// SafeToSync deliberately errs on the side of waiting. Open sessions always
// block sync, and only explicitly parked/asleep states are accepted.
func SafeToSync(ctx context.Context, db *sql.DB) (bool, error) {
	var open int
	if err := db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM drives WHERE status='open') +
		(SELECT COUNT(*) FROM charging_sessions WHERE status='open')`).Scan(&open); err != nil {
		return false, err
	}
	if open != 0 {
		return false, nil
	}
	var state string
	err := db.QueryRowContext(ctx, `SELECT state FROM states ORDER BY id DESC LIMIT 1`).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch strings.ToLower(state) {
	case "idle", "asleep", "offline", "suspended":
		return true, nil
	default:
		return false, nil
	}
}

func ReadStatus(ctx context.Context, db *sql.DB) (Status, error) {
	var s Status
	var started, completed, lastErr sql.NullString
	var manual int
	err := db.QueryRowContext(ctx, `SELECT sync_state,last_sync_started,last_sync_completed,
		last_sync_error,manual_sync_requested,
		(SELECT COUNT(*) FROM cloud_sync_changes WHERE state!='synced')
		FROM cloud_sync_status WHERE id=1`).Scan(&s.SyncState, &started, &completed, &lastErr, &manual, &s.PendingRows)
	if err != nil {
		return s, err
	}
	s.LastSyncStarted, s.LastSyncCompleted, s.LastSyncError = started.String, completed.String, lastErr.String
	s.ManualSyncRequested = manual != 0
	return s, nil
}

func RequestSync(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `UPDATE cloud_sync_status SET manual_sync_requested=1 WHERE id=1`)
	return err
}

func setStatus(ctx context.Context, db *sql.DB, state string, errText *string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	switch state {
	case "syncing":
		_, err := db.ExecContext(ctx, `UPDATE cloud_sync_status SET sync_state='syncing',last_sync_started=?,last_sync_error=NULL WHERE id=1`, now)
		return err
	case "idle":
		_, err := db.ExecContext(ctx, `UPDATE cloud_sync_status SET sync_state='idle',last_sync_completed=?,last_sync_error=NULL,manual_sync_requested=0 WHERE id=1`, now)
		return err
	case "waiting_for_idle":
		_, err := db.ExecContext(ctx, `UPDATE cloud_sync_status SET sync_state='waiting_for_idle' WHERE id=1`)
		return err
	case "failed":
		_, err := db.ExecContext(ctx, `UPDATE cloud_sync_status SET sync_state='failed',last_sync_error=? WHERE id=1`, *errText)
		return err
	default:
		return fmt.Errorf("unknown sync state %q", state)
	}
}

func pendingBatch(ctx context.Context, db *sql.DB, limit int) ([]Change, error) {
	rows, err := db.QueryContext(ctx, `SELECT sequence,table_name,row_id,operation
		FROM cloud_sync_changes WHERE state='pending' ORDER BY sequence LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.Sequence, &c.TableName, &c.RowID, &c.Operation); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func markBatch(ctx context.Context, db *sql.DB, changes []Change, state string) error {
	if len(changes) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range changes {
		var synced any
		if state == "synced" {
			synced = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE cloud_sync_changes SET state=?,synced_at=? WHERE sequence=?`, state, synced, c.Sequence); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func rowUpsert(ctx context.Context, db *sql.DB, table string, id int64) (string, error) {
	if !replicatedTables[table] {
		return "", fmt.Errorf("table %q is not allowed for cloud replication", table)
	}
	rows, err := db.QueryContext(ctx, `SELECT * FROM `+quoteIdent(table)+` WHERE id=?`, id)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return "", err
	}
	if !rows.Next() {
		return `DELETE FROM ` + quoteIdent(table) + ` WHERE id=` + fmt.Sprint(id), rows.Err()
	}
	values := make([]any, len(columns))
	ptrs := make([]any, len(columns))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return "", err
	}
	quoted := make([]string, len(columns))
	updates := make([]string, 0, len(columns)-1)
	for i, column := range columns {
		quoted[i] = quoteIdent(column)
		if column != "id" {
			updates = append(updates, quoted[i]+"=excluded."+quoted[i])
		}
	}
	return `INSERT INTO ` + quoteIdent(table) + ` (` + strings.Join(quoted, ",") + `) VALUES ` +
		encodeTuple(values) + ` ON CONFLICT(id) DO UPDATE SET ` + strings.Join(updates, ","), nil
}

// SyncIncremental sends only durable outbox entries. A batch is confirmed
// locally only after every cloud statement succeeds; uncertain batches are
// returned to pending and safely resent later.
func SyncIncremental(ctx context.Context, db *sql.DB, cfg Config, batchSize int) error {
	if batchSize <= 0 {
		batchSize = 500
	}
	client := New(cfg)
	for {
		safe, err := SafeToSync(ctx, db)
		if err != nil {
			return err
		}
		if !safe {
			return ErrVehicleBusy
		}
		batch, err := pendingBatch(ctx, db, batchSize)
		if err != nil || len(batch) == 0 {
			return err
		}
		if err := markBatch(ctx, db, batch, "syncing"); err != nil {
			return err
		}
		confirmed := false
		defer func() {
			if !confirmed {
				_ = markBatch(context.Background(), db, batch, "pending")
			}
		}()
		for _, change := range batch {
			safe, err := SafeToSync(ctx, db)
			if err != nil || !safe {
				if err != nil {
					return err
				}
				return ErrVehicleBusy
			}
			statement := `DELETE FROM ` + quoteIdent(change.TableName) + ` WHERE id=` + fmt.Sprint(change.RowID)
			if change.Operation != "delete" {
				statement, err = rowUpsert(ctx, db, change.TableName, change.RowID)
				if err != nil {
					return err
				}
			}
			if err := client.Exec(ctx, statement); err != nil {
				return err
			}
		}
		if err := markBatch(ctx, db, batch, "synced"); err != nil {
			return err
		}
		confirmed = true
	}
}

func CleanupAcknowledged(ctx context.Context, db *sql.DB, olderThan time.Duration) error {
	if olderThan <= 0 {
		olderThan = 24 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339Nano)
	_, err := db.ExecContext(ctx, `DELETE FROM cloud_sync_changes WHERE state='synced' AND synced_at < ?`, cutoff)
	return err
}
