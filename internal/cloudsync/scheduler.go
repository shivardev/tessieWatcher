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

func (s Scheduler) Start(ctx context.Context) {
	if s.Interval <= 0 {
		s.Interval = 15 * time.Minute
	}
	db, err := OpenLocal(s.DBPath)
	if err != nil {
		slog.Error("cloud sync disabled: open local database", "error", err)
		return
	}
	defer db.Close()
	if err := Recover(ctx, db); err != nil {
		slog.Error("cloud sync recovery failed", "error", err)
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
			slog.Error("cloud sync status failed", "error", err)
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
			slog.Error("cloud sync status update failed", "error", err)
		}
		_ = CleanupAcknowledged(ctx, db, 24*time.Hour)
		nextScheduled = time.Now().Add(s.Interval)
		slog.Info("incremental cloud sync complete", "database_id", s.Config.DatabaseID)
	}
}

func (s Scheduler) fail(ctx context.Context, db *sql.DB, syncErr error) {
	message := syncErr.Error()
	if err := setStatus(ctx, db, "failed", &message); err != nil {
		slog.Error("cloud sync status update failed", "error", err)
	}
	slog.Error("cloud sync failed", "error", syncErr)
}

func (s Scheduler) syncOnce(ctx context.Context, db *sql.DB) error {
	if err := PullGeofencesDB(ctx, db, s.Config); err != nil {
		return fmt.Errorf("pull cloud settings: %w", err)
	}
	if s.OnSettingsChanged != nil {
		s.OnSettingsChanged()
	}
	return SyncIncremental(ctx, db, s.Config, s.BatchSize)
}
