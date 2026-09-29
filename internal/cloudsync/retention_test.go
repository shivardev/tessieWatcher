package cloudsync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"teslalog/internal/storage"
)

func TestPruneAcknowledgedKeepsPendingAndRecentRows(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "buffer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := store.DB()
	old := time.Now().UTC().Add(-10 * 24 * time.Hour).Format(time.RFC3339Nano)
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO vehicles(id,vin) VALUES(1,'VIN')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO drives(id,vehicle_id,start_time,end_time,status) VALUES
		(1,1,?,?, 'closed'),(2,1,?,?, 'closed'),(3,1,?,?, 'closed')`, old, old, old, old, recent, recent); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO positions(id,drive_id,vehicle_id,timestamp) VALUES
		(10,1,1,?),(20,2,1,?),(30,3,1,?)`, old, old, recent); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE cloud_sync_changes SET state='synced',synced_at=?`, recent); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_meta(key,value) VALUES('cloud_sync_bootstrap_confirmed',?)`, recent); err != nil {
		t.Fatal(err)
	}
	// A newer unsynchronized edit makes row 20 ineligible even though an
	// earlier version of that row was acknowledged.
	if _, err := db.Exec(`UPDATE positions SET speed_kmh=42 WHERE id=20`); err != nil {
		t.Fatal(err)
	}

	deleted, err := PruneAcknowledged(context.Background(), db, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("pruned %d rows while an edit was pending", deleted)
	}
	if _, err := db.Exec(`UPDATE cloud_sync_changes SET state='synced',synced_at=? WHERE state<>'synced'`, recent); err != nil {
		t.Fatal(err)
	}
	deleted, err = PruneAcknowledged(context.Background(), db, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 4 { // two old positions followed by their empty drives
		t.Fatalf("deleted %d rows, want 4", deleted)
	}
	for id, want := range map[int]int{10: 0, 20: 0, 30: 1} {
		var got int
		if err := db.QueryRow(`SELECT COUNT(*) FROM positions WHERE id=?`, id).Scan(&got); err != nil || got != want {
			t.Fatalf("position %d count=%d want=%d err=%v", id, got, want, err)
		}
	}
	var retentionDeletes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cloud_sync_changes WHERE operation='delete'`).Scan(&retentionDeletes); err != nil {
		t.Fatal(err)
	}
	if retentionDeletes != 0 {
		t.Fatalf("local pruning leaked %d remote delete operations", retentionDeletes)
	}
}
