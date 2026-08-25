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

// nameLayout stamps backups in LOCAL time, with the UTC offset, e.g.
// teslalog-2026-08-25_030000-0400.db.gz.
//
// Local rather than UTC because the filename is read by a person, and
// "the backup from the morning of the 25th" is a local-time idea: a UTC
// date rolls over at 8pm in New York, so a UTC-named file disagrees with
// the day the user would call it. The offset is included so the name
// stays unambiguous across a DST change, when the same local hour occurs
// twice.
//
// Layout choices: no colons (illegal on Windows, awkward in shells), and
// the date first so the names sort chronologically in any file listing.
const nameLayout = "2006-01-02_150405-0700"

// backupGlob matches the files this package creates, and nothing else.
// Used to keep remote pruning from touching anything a person put in the
// same folder.
const backupGlob = "teslalog-*.db.gz"

// FileName is the backup filename for a moment in time. Exported so the
// portal and the uploader agree on the naming without duplicating it.
func FileName(at time.Time) string {
	return fmt.Sprintf("teslalog-%s.db.gz", at.Local().Format(nameLayout))
}

// Run performs one backup of the SQLite database at dbPath into
// backupDir (see FileName for the naming), then deletes backups older
// than retentionDays. Returns the path to the newly-created backup file.
func Run(ctx context.Context, dbPath, backupDir string, retentionDays int, at time.Time) (string, error) {
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}

	tmpPath := filepath.Join(backupDir, fmt.Sprintf(".tmp-backup-%d.db", at.UnixNano()))
	defer os.Remove(tmpPath)

	if err := Snapshot(ctx, dbPath, tmpPath); err != nil {
		return "", fmt.Errorf("sqlite backup: %w", err)
	}

	finalPath := filepath.Join(backupDir, FileName(at))
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
		t, err := parseBackupTime(e.Name())
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

// Scheduler runs backups in the background until ctx is canceled, then
// copies each one offsite.
type Scheduler struct {
	DBPath        string
	BackupDir     string
	RetentionDays int

	// Interval is the legacy cadence, used only when DailyAt is unset.
	Interval time.Duration

	// DailyAt is a local wall-clock time to run at, in "HH:MM" form.
	// Preferred over Interval: an interval counts from process start, so
	// every restart of the daemon moves the backup, and a Pi rebooted
	// each evening would only ever hold evening snapshots. Empty falls
	// back to Interval.
	DailyAt string

	// Uploader copies each finished backup offsite. Zero value means
	// local backups only.
	Uploader Uploader
}

// Start blocks until ctx is canceled, running backups on schedule.
// Intended to be run in its own goroutine.
func (s Scheduler) Start(ctx context.Context) {
	if ok, why := s.Uploader.Available(); ok {
		names := make([]string, 0, len(s.Uploader.Destinations))
		for _, d := range s.Uploader.Destinations {
			names = append(names, d.Name)
		}
		slog.Info("offsite backup copies enabled", "destinations", names)
	} else if len(s.Uploader.Destinations) > 0 {
		// Configured but unusable. Said once, loudly, at startup - the
		// alternative is discovering it from a missing file after the
		// disk you needed the backup for has already died.
		slog.Error("offsite backup copies are configured but cannot run", "reason", why)
	}

	// A first backup shortly after start, so a fresh install proves the
	// whole path works now rather than at 03:00 tomorrow.
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			s.runOnce(ctx, now)
			timer.Reset(s.nextDelay(time.Now()))
		}
	}
}

// runOnce takes one backup and copies it offsite. Upload failures are
// logged, never fatal: a local backup that exists is strictly better
// than no backup, and the next run will try the upload again.
func (s Scheduler) runOnce(ctx context.Context, now time.Time) {
	path, err := Run(ctx, s.DBPath, s.BackupDir, s.RetentionDays, now)
	if err != nil {
		slog.Error("scheduled backup failed", "error", err)
		return
	}
	slog.Info("backup complete", "path", path)

	if err := s.Uploader.Upload(ctx, path); err != nil {
		slog.Error("offsite backup copy incomplete", "error", err)
	}
}

// nextDelay is how long to wait before the next run. With DailyAt set,
// this is the gap to that local time tomorrow (or today, if it has not
// happened yet) - computed from the wall clock each time rather than by
// adding 24h, so it stays correct across a DST change instead of
// drifting an hour twice a year.
func (s Scheduler) nextDelay(now time.Time) time.Duration {
	if s.DailyAt == "" {
		return s.Interval
	}
	target, err := time.Parse("15:04", s.DailyAt)
	if err != nil {
		slog.Warn("invalid backup time, falling back to interval", "at", s.DailyAt, "error", err)
		return s.Interval
	}

	local := now.Local()
	next := time.Date(local.Year(), local.Month(), local.Day(), target.Hour(), target.Minute(), 0, 0, local.Location())
	if !next.After(local) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(local)
}

// parseBackupTime reads the timestamp back out of a backup filename.
//
// Accepts the older date-only UTC form as well as the current
// local-time-with-offset one, so upgrading does not orphan the backups
// already on disk - unrecognised names are skipped by prune, which would
// otherwise mean the old files were never cleaned up again.
func parseBackupTime(name string) (time.Time, error) {
	stamp := strings.TrimSuffix(strings.TrimPrefix(name, "teslalog-"), ".db.gz")
	if at, err := time.Parse(nameLayout, stamp); err == nil {
		return at, nil
	}
	return time.Parse("2006-01-02", stamp)
}
