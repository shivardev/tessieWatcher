package backup

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"database/sql"
	_ "github.com/ncruces/go-sqlite3/driver"
)

// seedDB creates a minimal SQLite database at path with one row, so tests
// can verify a snapshot/backup actually carries real data across, not
// just that some bytes got written somewhere.
func seedDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (42)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func readIntFromDB(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open db %s: %v", path, err)
	}
	defer db.Close()
	var x int
	if err := db.QueryRow(`SELECT x FROM t`).Scan(&x); err != nil {
		t.Fatalf("query db %s: %v", path, err)
	}
	return x
}

// TestSnapshotProducesAValidUsableCopy is a regression test for a real,
// serious bug: github.com/ncruces/go-sqlite3 v0.20.0's Conn.Backup crashed
// the entire process (a Windows access violation, reproduced and
// isolated down to this exact call) - not a Go panic, a hard runtime
// crash bypassing normal error handling entirely. Upgrading to v0.35.3
// fixed it. Since backup.enabled defaults to true, this would have
// crashed every real deployment ~30s after every start (the scheduler's
// initial delay), and systemd's Restart=always would just crash-loop
// forever. This test exists so a future accidental downgrade of
// go-sqlite3 gets caught here, not in production on the Pi.
func TestSnapshotProducesAValidUsableCopy(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	seedDB(t, srcPath)

	dstPath := filepath.Join(dir, "dst.db")
	if err := Snapshot(context.Background(), srcPath, dstPath); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if got := readIntFromDB(t, dstPath); got != 42 {
		t.Fatalf("expected snapshot to carry the source row (42), got %d", got)
	}
}

// TestSnapshotIsNotWALModeAndSupportsConcurrentReaders is a regression
// test for a real bug found live: the backup API's destination
// defaulted to WAL mode, same as the source. That's fine for a database
// something keeps writing to, but this snapshot is a static one-shot
// file with no matching -wal/-shm sidecars - opening it forces every
// reader through SQLite's WAL-recovery step, and concurrent readers
// (e.g. Grafana loading several dashboard panels against the same
// downloaded file at once - exactly what happened) race for that
// recovery and the loser gets SQLITE_BUSY_RECOVERY ("database is
// locked"), even though nothing is actually writing to the file.
func TestSnapshotIsNotWALModeAndSupportsConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	seedDB(t, srcPath)

	dstPath := filepath.Join(dir, "dst.db")
	if err := Snapshot(context.Background(), srcPath, dstPath); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	for _, sidecar := range []string{dstPath + "-wal", dstPath + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			t.Fatalf("expected no WAL sidecar file %s to exist after Snapshot", sidecar)
		}
	}

	db, err := sql.Open("sqlite3", "file:"+dstPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode == "wal" {
		t.Fatalf("expected snapshot to not be in WAL mode, got %q", mode)
	}

	// Simulate Grafana firing several panels' queries against the same
	// downloaded file at once - this must not produce "database is
	// locked" from any of them.
	errCh := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			conn, err := sql.Open("sqlite3", "file:"+dstPath+"?mode=ro")
			if err != nil {
				errCh <- err
				return
			}
			defer conn.Close()
			var x int
			errCh <- conn.QueryRow(`SELECT x FROM t`).Scan(&x)
		}()
	}
	for i := 0; i < 10; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent read %d failed: %v", i, err)
		}
	}
}

func TestRunProducesAGzippedRestorableBackup(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "tesla.db")
	seedDB(t, srcPath)

	backupDir := filepath.Join(dir, "backups")
	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	backupPath, err := Run(context.Background(), srcPath, backupDir, 30, at)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if filepath.Base(backupPath) != "teslalog-2026-08-20.db.gz" {
		t.Fatalf("unexpected backup filename: %s", backupPath)
	}

	gz, err := os.Open(backupPath)
	if err != nil {
		t.Fatalf("open backup file: %v", err)
	}
	defer gz.Close()
	r, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer r.Close()

	restoredPath := filepath.Join(dir, "restored.db")
	out, err := os.Create(restoredPath)
	if err != nil {
		t.Fatalf("create restored db: %v", err)
	}
	if _, err := io.Copy(out, r); err != nil {
		t.Fatalf("decompress backup: %v", err)
	}
	out.Close()

	if got := readIntFromDB(t, restoredPath); got != 42 {
		t.Fatalf("expected restored backup to carry the source row (42), got %d", got)
	}
}

func TestRunPrunesBackupsOlderThanRetention(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "tesla.db")
	seedDB(t, srcPath)
	backupDir := filepath.Join(dir, "backups")

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	const retentionDays = 5

	var lastPath string
	for i := 0; i <= 10; i++ {
		at := base.AddDate(0, 0, i)
		p, err := Run(context.Background(), srcPath, backupDir, retentionDays, at)
		if err != nil {
			t.Fatalf("Run day %d: %v", i, err)
		}
		lastPath = p
	}

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	if len(entries) > retentionDays+1 {
		t.Fatalf("expected pruning to keep at most %d backups, found %d", retentionDays+1, len(entries))
	}
	if _, err := os.Stat(lastPath); err != nil {
		t.Fatalf("expected the most recent backup to survive pruning: %v", err)
	}
}
