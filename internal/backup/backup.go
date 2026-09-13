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

// Snapshot writes a consistent, self-contained copy of the live database
// at srcPath to dstPath using SQLite's `VACUUM INTO`, run from a separate
// read-only connection so it never blocks (or is blocked by) the daemon's
// own writes.
//
// This replaced the online backup API (sqlite3_backup_init/_step/_finish
// via Conn.Backup). The backup API restarts its copy from the beginning
// every time a concurrent writer commits to the source - and teslalog
// writes to tesla.db every few seconds while a car is awake, especially
// while charging - so on the live Pi a backup could stall indefinitely,
// never converging. `VACUUM INTO` instead runs inside a single read
// transaction: it takes one consistent view of the database at the moment
// it starts and is immune to concurrent-write restarts (validated on the
// Pi under an active charging session). It is a single pass, and unlike
// the backup API it produces a freshly-packed database in the connection's
// default rollback (DELETE) journal mode, not WAL - so there is no second
// journal-mode fixup pass and no -wal/-shm sidecar to lose.
//
// The DELETE-mode output matters for portability: a WAL-flagged .db with
// no sidecar files forces every reader to run WAL recovery on first open,
// and concurrent readers (e.g. Grafana loading a dashboard's several
// panels at once) race for that recovery and lose with
// SQLITE_BUSY_RECOVERY ("database is locked"). VACUUM INTO's compacted,
// single-file DELETE-mode output is the most portable format for any
// external tool to open, and immune to that race.
//
// Exported so callers other than Run (e.g. internal/portal's download
// snapshot cache) can get a consistent, safe copy of the live database
// without going through Run's gzip+rotation-into-backupDir behavior.
func Snapshot(ctx context.Context, srcPath, dstPath string) error {
	// VACUUM INTO refuses to overwrite an existing file. The portal cache
	// writes to a temp path it controls, but guard here so a stale file
	// never turns a snapshot into a silent failure.
	if err := os.Remove(dstPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear snapshot destination: %w", err)
	}

	src, err := sqlite3.OpenContext(ctx, "file:"+srcPath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer src.Close()

	// The destination path is a string literal in the SQL. Single-quotes
	// are the only character that could break out of it; double them, the
	// standard SQL escape, so an unusual temp path can't corrupt the
	// statement.
	quoted := strings.ReplaceAll(dstPath, "'", "''")
	if err := src.Exec("VACUUM INTO '" + quoted + "'"); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	return nil
}

// GzipFileLevel writes a gzip-compressed copy of srcPath to dstPath at the
// given compression level (see compress/gzip constants). Exported for the
// portal's download cache: it compresses at gzip.BestSpeed, because that
// runs several times a day (once per finished trip) on a Pi Zero 2 W's
// slow CPU, where BestSpeed is ~2-3x faster than the default level for a
// SQLite file that still shrinks roughly fourfold - the transfer saving
// that matters, without the CPU cost of squeezing out the last few percent.
func GzipFileLevel(srcPath, dstPath string, level int) error {
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

	gw, err := gzip.NewWriterLevel(dst, level)
	if err != nil {
		return err
	}
	if _, err := io.Copy(gw, src); err != nil {
		gw.Close()
		return err
	}
	return gw.Close()
}

// gzipFile compresses at the default level, used by the daily offsite
// backup where smaller matters more than speed (it runs once, overnight).
func gzipFile(srcPath, dstPath string) error {
	return GzipFileLevel(srcPath, dstPath, gzip.DefaultCompression)
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
