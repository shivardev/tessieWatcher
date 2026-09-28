package cloudsync

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"teslalog/internal/pgserver"
	"teslalog/internal/storage"
)

// TestPostgresBootstrapAndCatchUp is opt-in because ordinary development and
// CI do not require Docker. Set TESLALOG_TEST_POSTGRES_DSN to an empty,
// disposable database; the test verifies bootstrap, idempotent retry, update,
// delete, and outage catch-up through the real HTTP receiver.
func TestPostgresBootstrapAndCatchUp(t *testing.T) {
	dsn := os.Getenv("TESLALOG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TESLALOG_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	server, err := pgserver.New(ctx, pgserver.Config{DSN: dsn, DatabaseID: "teslalog", Token: "integration-secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	store, err := storage.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.DB().Exec(`
		INSERT INTO vehicles(id,vin,display_name) VALUES(1,'TESTVIN','Before sync');
		INSERT INTO states(id,vehicle_id,state,started_at) VALUES(1,1,'offline','2026-09-28T00:00:00Z');
		INSERT INTO battery_samples(id,vehicle_id,timestamp,battery_level,source)
		VALUES(1,1,'2026-09-28T00:00:00Z',50,'poll');`); err != nil {
		t.Fatal(err)
	}

	cfg := Config{BaseURL: httpServer.URL, DatabaseID: "teslalog", APIKey: "integration-secret"}
	if err := EnsureBootstrap(ctx, store.DB(), cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := SyncIncremental(ctx, store.DB(), cfg, 100); err != nil {
		t.Fatalf("idempotent initial sync: %v", err)
	}

	if _, err := store.DB().Exec(`
		UPDATE vehicles SET display_name='After sync' WHERE id=1;
		INSERT INTO battery_samples(id,vehicle_id,timestamp,battery_level,source)
		VALUES(2,1,'2026-09-28T00:01:00Z',51,'poll');
		DELETE FROM battery_samples WHERE id=1;`); err != nil {
		t.Fatal(err)
	}
	bad := cfg
	bad.BaseURL = "http://127.0.0.1:1"
	failedCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	err = SyncIncremental(failedCtx, store.DB(), bad, 100)
	cancel()
	if err == nil {
		t.Fatal("expected disconnected receiver to fail")
	}
	var pending int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM cloud_sync_changes WHERE state='pending'`).Scan(&pending); err != nil || pending == 0 {
		t.Fatalf("outage must retain pending changes: pending=%d err=%v", pending, err)
	}
	if err := SyncIncremental(ctx, store.DB(), cfg, 100); err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	client := New(cfg)
	rows, err := client.queryRows(ctx, `SELECT display_name,
		(SELECT COUNT(*) FROM battery_samples WHERE id=1) deleted,
		(SELECT COUNT(*) FROM battery_samples WHERE id=2) inserted
		FROM vehicles WHERE id=1`)
	if err != nil || len(rows) != 1 || textValue(rows[0]["display_name"]) != "After sync" ||
		integer(rows[0]["deleted"]) != 0 || integer(rows[0]["inserted"]) != 1 {
		t.Fatalf("unexpected receiver state: rows=%#v err=%v", rows, err)
	}
}
