package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"teslalog/internal/cloudsync"
	"teslalog/internal/storage"
)

func TestSyncAdminLinksPiAndQueuesInitialSync(t *testing.T) {
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"columns":["1"],"rows":[[1]],"rowCount":1}`))
	}))
	defer receiver.Close()

	path := filepath.Join(t.TempDir(), "teslalog.db")
	store, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller, err := cloudsync.NewController(ctx, path, "", cloudsync.Settings{
		DatabaseID: "teslalog", Interval: 15 * time.Minute, BatchSize: 500,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	server := New(store, path, nil, "metric", "test")
	server.SetSyncController(controller)
	body := `{"enabled":true,"base_url":"` + receiver.URL + `","database_id":"teslalog","interval_seconds":900,"batch_size":500}`
	req := httptest.NewRequest(http.MethodPost, "/api/sync/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("configure status %d: %s", rec.Code, rec.Body.String())
	}
	if settings := controller.Settings(); !settings.Enabled || settings.BaseURL != receiver.URL {
		t.Fatalf("settings were not applied: %+v", settings)
	}
	status, err := cloudsync.ReadStatus(context.Background(), store.DB())
	if err != nil {
		t.Fatal(err)
	}
	if !status.ManualSyncRequested {
		t.Fatal("expected linking to queue the initial synchronization")
	}
}

func TestSyncAdminPageIsAvailable(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "teslalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := New(store, "unused.db", nil, "metric", "test")
	rec := httptest.NewRecorder()
	server.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/sync", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Database synchronization") {
		t.Fatalf("admin page response %d: %s", rec.Code, rec.Body.String())
	}
}
