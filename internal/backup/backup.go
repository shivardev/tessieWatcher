// Package backup performs safe, online SQLite backups (using SQLite's
// backup API, not a raw file copy, so it can't race with an in-flight
// WAL write) and gzips + rotates them.
package backup

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
)

// Run performs one backup of the SQLite database at dbPath into
// backupDir, named teslalog-YYYY-MM-DD.db.gz (UTC date of at), then
// deletes backups older than retentionDays. Returns the path to the
// newly-created backup file.
func Run(ctx context.Context, dbPath, backupDir string, retentionDays int, at time.Time) (string, error) {
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}

	tmpPath := filepath.Join(backupDir, fmt.Sprintf(".tmp-backup-%d.db", at.UnixNano()))
	defer os.Remove(tmpPath)

	if err := Snapshot(ctx, dbPath, tmpPath); err != nil {
		return "", fmt.Errorf("sqlite backup: %w", err)
	}

	finalPath := filepath.Join(backupDir, fmt.Sprintf("teslalog-%s.db.gz", at.UTC().Format("2006-01-02")))
	if err := gzipFile(tmpPath, finalPath); err != nil {
		return "", fmt.Errorf("gzip backup: %w", err)
	}

	if err := prune(backupDir, retentionDays, at); err != nil {
		// Non-fatal: the backup itself succeeded.
		slog.Warn("backup retention pruning failed", "error", err)
	}

	return finalPath, nil
}

// Snapshot uses SQLite's online backup API (sqlite3_backup_init /
// _step / _finish), via ncruces/go-sqlite3's native Conn.Backup, which
// is safe to run against a live WAL-mode database that teslalog is
// actively writing to (this opens its own connection to srcPath rather
// than sharing the daemon's, so it never blocks/is blocked by it).
// Exported so callers other than Run (e.g. internal/portal's on-demand
// download) can get a consistent, safe copy of the live database without
// going through Run's gzip+rotation-into-backupDir behavior.
func Snapshot(ctx context.Context, srcPath, dstPath string) error {
	src, err := sqlite3.OpenContext(ctx, "file:"+srcPath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer src.Close()

	if err := src.Backup("main", dstPath); err != nil {
		return fmt.Errorf("backup: %w", err)
	}

	// The backup API creates dstPath as a fresh database, which defaults
	// to WAL mode (matching the live one) - fine for a database teslalog
	// itself keeps writing to, but this snapshot is a static, one-shot
	// artifact nothing ever writes to again. Left in WAL mode, it needs
	// matching -wal/-shm sidecar files that this function doesn't
	// produce (and that get lost anyway once this file is copied
	// elsewhere, e.g. downloaded via the portal) - opening a WAL-flagged
	// .db with no sidecar files forces every reader to perform WAL
	// recovery on first open, and concurrent readers (e.g. Grafana
	// loading a dashboard's several panels at once) race for that
	// recovery and lose with SQLITE_BUSY_RECOVERY ("database is
	// locked"). Switching to DELETE mode checkpoints and folds
	// everything into the single main file - the most portable format
	// for any external tool to open, and immune to this race.
	dst, err := sqlite3.OpenContext(ctx, "file:"+dstPath)
	if err != nil {
		return fmt.Errorf("open snapshot for journal-mode fixup: %w", err)
	}
	defer dst.Close()
	if err := dst.Exec("PRAGMA journal_mode = DELETE"); err != nil {
		return fmt.Errorf("set snapshot journal mode: %w", err)
	}
	return nil
}

func gzipFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	gw := gzip.NewWriter(dst)
	if _, err := io.Copy(gw, src); err != nil {
		gw.Close()
		return err
	}
	return gw.Close()
}

// prune deletes teslalog-*.db.gz files in dir older than retentionDays
// relative to now. retentionDays <= 0 disables pruning.
func prune(dir string, retentionDays int, now time.Time) error {
	if retentionDays <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	cutoff := now.AddDate(0, 0, -retentionDays)
	var removed []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "teslalog-") || !strings.HasSuffix(e.Name(), ".db.gz") {
			continue
		}
		datePart := strings.TrimSuffix(strings.TrimPrefix(e.Name(), "teslalog-"), ".db.gz")
		t, err := time.Parse("2006-01-02", datePart)
		if err != nil {
			continue // don't touch files we don't recognize
		}
		if t.Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				removed = append(removed, e.Name())
			}
		}
	}
	sort.Strings(removed)
	if len(removed) > 0 {
		slog.Info("pruned old backups", "count", len(removed), "files", removed)
	}
	return nil
}

// Scheduler runs Run on a fixed interval in the background until ctx is
// canceled. It runs once shortly after Start (30s, to let the daemon
// finish initializing) and then every interval.
type Scheduler struct {
	DBPath        string
	BackupDir     string
	RetentionDays int
	Interval      time.Duration
}

// Start blocks until ctx is canceled, running backups on schedule.
// Intended to be run in its own goroutine.
func (s Scheduler) Start(ctx context.Context) {
	initialDelay := 30 * time.Second
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			path, err := Run(ctx, s.DBPath, s.BackupDir, s.RetentionDays, now)
			if err != nil {
				slog.Error("scheduled backup failed", "error", err)
			} else {
				slog.Info("backup complete", "path", path)
			}
			timer.Reset(s.Interval)
		}
	}
}
