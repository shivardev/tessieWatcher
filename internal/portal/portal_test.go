package portal

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"teslalog/internal/storage"
)

func openTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIndexShowsVehicleAndTodayStats(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	vehicleID, err := store.UpsertVehicle(storage.VehicleMeta{VIN: "VIN1", DisplayName: "My Model 3"})
	if err != nil {
		t.Fatalf("upsert vehicle: %v", err)
	}
	now := time.Now().UTC()
	driveID, err := store.OpenDrive(storage.DriveStart{VehicleID: vehicleID, Time: now, OdometerKm: 100, BatteryLevel: 80})
	if err != nil {
		t.Fatalf("open drive: %v", err)
	}
	if err := store.CloseDrive(storage.DriveEnd{DriveID: driveID, Time: now.Add(10 * time.Minute), OdometerKm: 110, BatteryLevel: 75}); err != nil {
		t.Fatalf("close drive: %v", err)
	}

	srv := New(store, dbPath, nil)
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "My Model 3") {
		t.Fatalf("expected vehicle name in page, got: %s", body)
	}
	if !strings.Contains(body, "10.0 km") {
		t.Fatalf("expected today's 10.0 km distance in page, got: %s", body)
	}
	if !strings.Contains(body, "Download database") {
		t.Fatalf("expected a download button, got: %s", body)
	}
}

func TestIndexWithNoVehicleYetDoesNotError(t *testing.T) {
	store := openTestStore(t)
	srv := New(store, "unused.db", nil)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200 even with no vehicle recorded yet, got %d", rec.Code)
	}
}

func TestDownloadServesAValidSQLiteSnapshot(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if _, err := store.UpsertVehicle(storage.VehicleMeta{VIN: "VIN1", DisplayName: "My Model 3"}); err != nil {
		t.Fatalf("upsert vehicle: %v", err)
	}

	srv := New(store, dbPath, nil)
	req := httptest.NewRequest("GET", "/download", nil)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".db") {
		t.Fatalf("expected an attachment Content-Disposition ending in .db, got %q", cd)
	}

	// The served bytes must be a real, openable SQLite database with our
	// vehicle row in it - not just "some bytes got sent".
	snapshotPath := filepath.Join(dir, "downloaded.db")
	if err := os.WriteFile(snapshotPath, rec.Body.Bytes(), 0o644); err != nil {
		t.Fatalf("write downloaded snapshot: %v", err)
	}
	verify, err := storage.Open(snapshotPath)
	if err != nil {
		t.Fatalf("open downloaded snapshot as sqlite: %v", err)
	}
	defer verify.Close()

	var displayName string
	if err := verify.DB().QueryRow(`SELECT display_name FROM vehicles WHERE vin = ?`, "VIN1").Scan(&displayName); err != nil {
		t.Fatalf("query downloaded snapshot: %v", err)
	}
	if displayName != "My Model 3" {
		t.Fatalf("expected downloaded snapshot to contain our vehicle, got %q", displayName)
	}
}

func TestIndexShowsCurrentStateAndRecentActivity(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	vehicleID, err := store.UpsertVehicle(storage.VehicleMeta{VIN: "VIN1", DisplayName: "My Model 3"})
	if err != nil {
		t.Fatalf("upsert vehicle: %v", err)
	}
	if _, err := store.OpenState(vehicleID, "driving", time.Now().UTC()); err != nil {
		t.Fatalf("open state: %v", err)
	}

	logs := NewLogBuffer(10)
	logger := slog.New(logs.Handler())
	logger.Info("drive started", "drive_id", 1)

	srv := New(store, dbPath, logs)
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "driving") {
		t.Fatalf("expected current state 'driving' on the page, got: %s", body)
	}
	if !strings.Contains(body, "drive started") {
		t.Fatalf("expected the recent log line on the page, got: %s", body)
	}
}

func TestIndexWithNoStateYetShowsPlaceholder(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.UpsertVehicle(storage.VehicleMeta{VIN: "VIN1", DisplayName: "My Model 3"}); err != nil {
		t.Fatalf("upsert vehicle: %v", err)
	}

	srv := New(store, "unused.db", nil)
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "not seen yet") {
		t.Fatalf("expected a placeholder when no state has been recorded yet, got: %s", rec.Body.String())
	}
}

func TestIndexShowsRecentDrivesWithLocations(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	vehicleID, err := store.UpsertVehicle(storage.VehicleMeta{VIN: "VIN1", DisplayName: "My Model 3"})
	if err != nil {
		t.Fatalf("upsert vehicle: %v", err)
	}
	now := time.Now().UTC()
	driveID, err := store.OpenDrive(storage.DriveStart{
		VehicleID: vehicleID, Time: now, OdometerKm: 100, BatteryLevel: 80, StartLocation: "Home",
	})
	if err != nil {
		t.Fatalf("open drive: %v", err)
	}
	if err := store.CloseDrive(storage.DriveEnd{
		DriveID: driveID, Time: now.Add(10 * time.Minute), OdometerKm: 108, BatteryLevel: 74, EndLocation: "Work",
	}); err != nil {
		t.Fatalf("close drive: %v", err)
	}

	srv := New(store, dbPath, nil)
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Home") || !strings.Contains(body, "Work") {
		t.Fatalf("expected both drive locations on the page, got: %s", body)
	}
	if !strings.Contains(body, "Recent drives") {
		t.Fatalf("expected a 'Recent drives' section, got: %s", body)
	}
}
