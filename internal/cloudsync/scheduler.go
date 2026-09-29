package cloudsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Scheduler performs low-priority incremental replication. It never creates a
// database snapshot and refuses to work while a drive or charge is active.
type Scheduler struct {
	DBPath            string
	Config            Config
	Interval          time.Duration
	BatchSize         int
	OnSettingsChanged func()
}

const localBufferRetention = 7 * 24 * time.Hour

func (s Scheduler) Start(ctx context.Context) {
	if s.Interval <= 0 {
		s.Interval = 15 * time.Minute
	}
	db, err := OpenLocal(s.DBPath)
	if err != nil {
		slog.Error("replication disabled: open local database", "error", err)
		return
	}
	defer db.Close()
	if err := Recover(ctx, db); err != nil {
		slog.Error("replication recovery failed", "error", err)
		return
	}

	check := time.NewTicker(5 * time.Second)
	defer check.Stop()
	nextScheduled := time.Now().Add(10 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-check.C:
		}
		status, err := ReadStatus(ctx, db)
		if err != nil {
			slog.Error("replication status failed", "error", err)
			continue
		}
		if !status.ManualSyncRequested && time.Now().Before(nextScheduled) {
			continue
		}
		safe, err := SafeToSync(ctx, db)
		if err != nil {
			s.fail(ctx, db, err)
			nextScheduled = time.Now().Add(s.Interval)
			continue
		}
		if !safe {
			_ = setStatus(ctx, db, "waiting_for_idle", nil)
			continue
		}
		if err := setStatus(ctx, db, "syncing", nil); err != nil {
			continue
		}
		if err := s.syncOnce(ctx, db); err != nil {
			if errors.Is(err, ErrVehicleBusy) {
				_ = setStatus(ctx, db, "waiting_for_idle", nil)
				continue
			}
			s.fail(ctx, db, err)
			nextScheduled = time.Now().Add(s.Interval)
			continue
		}
		if err := setStatus(ctx, db, "idle", nil); err != nil {
			slog.Error("replication status update failed", "error", err)
		}
		if deleted, err := PruneAcknowledged(ctx, db, localBufferRetention); err != nil {
			slog.Error("local buffer pruning failed", "error", err)
		} else if deleted > 0 {
			slog.Info("pruned acknowledged rows from local buffer", "rows", deleted, "retention", localBufferRetention)
		}
		// Keep acknowledgement evidence until it has had time to authorize
		// retention pruning. Removing it after only 24 hours would make an
		// older row impossible to prove safe to delete locally.
		_ = CleanupAcknowledged(ctx, db, localBufferRetention+24*time.Hour)
		nextScheduled = time.Now().Add(s.Interval)
		slog.Info("incremental replication complete", "database_id", s.Config.DatabaseID)
	}
}

func (s Scheduler) fail(ctx context.Context, db *sql.DB, syncErr error) {
	message := syncErr.Error()
	if err := setStatus(ctx, db, "failed", &message); err != nil {
		slog.Error("replication status update failed", "error", err)
	}
	slog.Error("replication failed", "error", syncErr)
}

func (s Scheduler) syncOnce(ctx context.Context, db *sql.DB) error {
	if err := EnsureBootstrap(ctx, db, s.Config); err != nil {
		return fmt.Errorf("bootstrap receiver: %w", err)
	}
	// EnsureBootstrap returns only after the receiver confirms its durable
	// initial-bootstrap marker. Record that proof locally so pre-outbox rows
	// can later age out of the Pi buffer without weakening pending-row safety.
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_meta(key,value) VALUES('cloud_sync_bootstrap_confirmed',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record bootstrap confirmation: %w", err)
	}
	if err := PullGeofencesDB(ctx, db, s.Config); err != nil {
		return fmt.Errorf("pull replicated settings: %w", err)
	}
	if s.OnSettingsChanged != nil {
		s.OnSettingsChanged()
	}
	return SyncIncremental(ctx, db, s.Config, s.BatchSize)
}
