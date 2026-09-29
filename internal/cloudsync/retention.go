package cloudsync

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PruneAcknowledged bounds the Pi's SQLite database after a successful sync.
// It removes only closed/history rows older than retention for which the
// durable outbox proves every change was acknowledged. The delete-trigger
// entries created by this local retention operation are removed in the same
// transaction, so pruning the Pi can never delete the PostgreSQL archive.
func PruneAcknowledged(ctx context.Context, db *sql.DB, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339Nano)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var unacknowledged int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM cloud_sync_changes WHERE state<>'synced'`).Scan(&unacknowledged); err != nil {
		return 0, err
	}
	if unacknowledged != 0 {
		return 0, tx.Rollback()
	}
	var bootstrapConfirmed int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_meta WHERE key='cloud_sync_bootstrap_confirmed'`).Scan(&bootstrapConfirmed); err != nil {
		return 0, err
	}
	if bootstrapConfirmed == 0 {
		return 0, tx.Rollback()
	}

	var watermark int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM cloud_sync_changes`).Scan(&watermark); err != nil {
		return 0, err
	}
	// Disable only delete capture inside this transaction. DDL is
	// transactional in SQLite: a rollback restores every trigger, while the
	// single-writer connection prevents a user delete from slipping through.
	// This avoids manufacturing hundreds of thousands of throwaway outbox
	// rows during the first retention pass on an established database.
	deleteTriggerTables := []string{"states", "drives", "positions", "charging_sessions", "charging_samples", "battery_samples", "software_updates"}
	for _, table := range deleteTriggerTables {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+quoteIdent("cloud_sync_"+table+"_delete")); err != nil {
			return 0, err
		}
	}

	type pruneSpec struct {
		table, timeColumn, extra string
	}
	// Child tables must be removed before their parent summaries because
	// foreign keys are intentionally restrictive rather than cascading.
	specs := []pruneSpec{
		{"positions", "timestamp", `local_row.id <> (SELECT COALESCE(MAX(id),0) FROM positions)`},
		{"charging_samples", "timestamp", `local_row.id <> (SELECT COALESCE(MAX(id),0) FROM charging_samples)`},
		{"battery_samples", "timestamp", `local_row.id <> (SELECT COALESCE(MAX(id),0) FROM battery_samples)`},
		{"states", "ended_at", `local_row.ended_at IS NOT NULL AND local_row.id <> (SELECT COALESCE(MAX(id),0) FROM states)`},
		{"software_updates", "end_time", `local_row.end_time IS NOT NULL AND local_row.id <> (SELECT COALESCE(MAX(id),0) FROM software_updates)`},
		{"drives", "end_time", `local_row.status='closed' AND local_row.id <> (SELECT COALESCE(MAX(id),0) FROM drives) AND NOT EXISTS (SELECT 1 FROM positions p WHERE p.drive_id=local_row.id)`},
		{"charging_sessions", "end_time", `local_row.status='closed' AND local_row.id <> (SELECT COALESCE(MAX(id),0) FROM charging_sessions) AND NOT EXISTS (SELECT 1 FROM charging_samples s WHERE s.charging_session_id=local_row.id)`},
	}

	var total int64
	for _, spec := range specs {
		statement := fmt.Sprintf(`DELETE FROM %s AS local_row WHERE %s < ? AND (%s)`,
			quoteIdent(spec.table), quoteIdent(spec.timeColumn), spec.extra)
		result, err := tx.ExecContext(ctx, statement, cutoff)
		if err != nil {
			return 0, fmt.Errorf("prune %s: %w", spec.table, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		total += count
	}

	// These are local-retention deletes, not user deletes. Discard only the
	// trigger records created after our transaction watermark.
	if _, err := tx.ExecContext(ctx, `DELETE FROM cloud_sync_changes WHERE sequence > ? AND operation='delete'`, watermark); err != nil {
		return 0, err
	}
	for _, table := range deleteTriggerTables {
		statement := fmt.Sprintf(`CREATE TRIGGER %s AFTER DELETE ON %s BEGIN INSERT INTO cloud_sync_changes(table_name,row_id,operation) VALUES('%s',OLD.id,'delete'); END`,
			quoteIdent("cloud_sync_"+table+"_delete"), quoteIdent(table), table)
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}
