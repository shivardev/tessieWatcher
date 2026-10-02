package cloudsync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"teslalog/internal/storage"
)

func TestControllerPersistsBrowserManagedSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "teslalog.db")
	store, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	controller, err := NewController(context.Background(), path, "", Settings{
		DatabaseID: "teslalog", Interval: 15 * time.Minute, BatchSize: 500,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{BaseURL: "http://192.168.1.50:8085/", DatabaseID: "fleet", Interval: 5 * time.Minute, BatchSize: 250}
	if err := controller.Configure(context.Background(), want); err != nil {
		t.Fatal(err)
	}

	db, err := OpenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := readSettings(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != "http://192.168.1.50:8085" || got.DatabaseID != "fleet" || got.Interval != 5*time.Minute || got.BatchSize != 250 || got.Enabled {
		t.Fatalf("unexpected persisted settings: %+v", got)
	}
}

func TestControllerRejectsInvalidEnabledDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "teslalog.db")
	store, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	controller, err := NewController(context.Background(), path, "", Settings{DatabaseID: "teslalog"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Configure(context.Background(), Settings{Enabled: true, BaseURL: "not a URL", DatabaseID: "teslalog"}); err == nil {
		t.Fatal("expected an invalid enabled destination to be rejected")
	}
}
