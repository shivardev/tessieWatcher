package cloudsync

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"teslalog/internal/dbserver"
	"teslalog/internal/storage"
)

func TestEnsureBootstrapCopiesExistingHistoryAndMarksReceiver(t *testing.T) {
	localStore, err := storage.Open(filepath.Join(t.TempDir(), "primary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer localStore.Close()
	if _, err := localStore.DB().Exec(`INSERT INTO vehicles(id,vin,display_name) VALUES(7,'VIN7','Existing car')`); err != nil {
		t.Fatal(err)
	}
	if _, err := localStore.DB().Exec(`INSERT INTO states(id,vehicle_id,state,started_at) VALUES(11,7,'offline','2026-09-22T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	receiver, err := dbserver.New(dbserver.Config{
		DatabasePath: filepath.Join(t.TempDir(), "replica.db"), DatabaseID: "teslalog", Token: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	httpServer := httptest.NewServer(receiver.Handler())
	defer httpServer.Close()
	cfg := Config{BaseURL: httpServer.URL, DatabaseID: "teslalog", APIKey: "secret"}

	if err := EnsureBootstrap(context.Background(), localStore.DB(), cfg); err != nil {
		t.Fatal(err)
	}
	client := New(cfg)
	rows, err := client.queryRows(context.Background(), `SELECT id,display_name FROM vehicles`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || integer(rows[0]["id"]) != 7 || textValue(rows[0]["display_name"]) != "Existing car" {
		t.Fatalf("unexpected receiver vehicles: %#v", rows)
	}
	markers, err := client.queryRows(context.Background(), `SELECT completed_at FROM teslalog_replication_meta WHERE name='initial_bootstrap'`)
	if err != nil || len(markers) != 1 {
		t.Fatalf("bootstrap marker: rows=%#v err=%v", markers, err)
	}

	// A second run is a no-op rather than duplicating or rebuilding history.
	if err := EnsureBootstrap(context.Background(), localStore.DB(), cfg); err != nil {
		t.Fatal(err)
	}
	rows, err = client.queryRows(context.Background(), `SELECT COUNT(*) AS count FROM vehicles`)
	if err != nil || len(rows) != 1 || integer(rows[0]["count"]) != 1 {
		t.Fatalf("idempotent bootstrap count: rows=%#v err=%v", rows, err)
	}
}
