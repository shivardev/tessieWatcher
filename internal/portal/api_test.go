package portal

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"teslalog/internal/storage"
)

func TestAPIStatusReportsVehicleAndBattery(t *testing.T) {
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
	if _, err := store.OpenState(vehicleID, "driving", now); err != nil {
		t.Fatalf("open state: %v", err)
	}
	if err := store.InsertBatterySample(vehicleID, now, 72, 250.5, 260.0, "poll"); err != nil {
		t.Fatalf("insert battery sample: %v", err)
	}
	driveID, err := store.OpenDrive(storage.DriveStart{VehicleID: vehicleID, Time: now, OdometerKm: 100, BatteryLevel: 80})
	if err != nil {
		t.Fatalf("open drive: %v", err)
	}

	srv := New(store, dbPath, nil, "metric", "test")
	req := httptest.NewRequest("GET", "/api/status", nil)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("expected JSON content type, got %q", ct)
	}

	var out apiStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v (body: %s)", err, rec.Body.String())
	}
	if out.VehicleName != "My Model 3" {
		t.Fatalf("expected vehicle name, got %q", out.VehicleName)
	}
	if out.State != "driving" {
		t.Fatalf("expected state 'driving', got %q", out.State)
	}
	if out.BatteryLevel == nil || *out.BatteryLevel != 72 {
		t.Fatalf("expected battery level 72, got %v", out.BatteryLevel)
	}
	if out.ActiveDriveID == nil || *out.ActiveDriveID != driveID {
		t.Fatalf("expected active drive id %d, got %v", driveID, out.ActiveDriveID)
	}
	if out.ActiveChargeID != nil {
		t.Fatalf("expected no active charge, got %v", out.ActiveChargeID)
	}
}

func TestAPIStatusWithNoVehicleYetDoesNotError(t *testing.T) {
	store := openTestStore(t)
	srv := New(store, "unused.db", nil, "metric", "test")

	req := httptest.NewRequest("GET", "/api/status", nil)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var out apiStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if out.VehicleName != "Vehicle" {
		t.Fatalf("expected default vehicle name, got %q", out.VehicleName)
	}
}

func TestAPIMetaReportsFreshness(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	srv := New(store, dbPath, nil, "metric", "test")
	req := httptest.NewRequest("GET", "/api/meta", nil)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var out struct {
		LastUpdated string `json:"last_updated"`
		SizeBytes   int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if out.SizeBytes <= 0 {
		t.Fatalf("expected a positive size, got %d", out.SizeBytes)
	}
	if _, err := time.Parse(time.RFC3339Nano, out.LastUpdated); err != nil {
		t.Fatalf("expected parseable RFC3339 timestamp, got %q: %v", out.LastUpdated, err)
	}
}

func TestCORSHeadersPresentOnEveryRoute(t *testing.T) {
	store := openTestStore(t)
	srv := New(store, "unused.db", nil, "metric", "test")

	for _, path := range []string{"/", "/api/status", "/api/meta"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("%s: expected wildcard CORS header, got %q", path, got)
		}
	}
}

func TestCORSPreflightShortCircuits(t *testing.T) {
	store := openTestStore(t)
	srv := New(store, "unused.db", nil, "metric", "test")

	req := httptest.NewRequest("OPTIONS", "/api/status", nil)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != 204 {
		t.Fatalf("expected 204 No Content for preflight, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected wildcard CORS header on preflight, got %q", got)
	}
}

// TestAPIStatusReportsRunningVersion pins that the portal surfaces the
// running build. Without it, "did my update actually land?" can only
// be answered by SSH-ing in or by inferring the version from the
// database's schema shape - which is exactly what had to be done in
// practice, and is a poor diagnostic for a daemon whose main update
// path is a self-updating binary.
func TestAPIStatusReportsRunningVersion(t *testing.T) {
	store := openTestStore(t)
	srv := New(store, "unused.db", nil, "metric", "9.9.9")

	req := httptest.NewRequest("GET", "/api/status", nil)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	var out apiStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if out.Version != "9.9.9" {
		t.Fatalf("expected the running version in /api/status, got %q", out.Version)
	}
}

// TestAPIMetaReportsChangeCounters pins the counters a client uses to
// decide whether re-downloading the whole database is worthwhile.
// /download takes a fresh full snapshot every time (~1s and ~10MB on a
// Pi Zero 2 W); polling it on a timer would be gigabytes per day of
// transfer and SD-card writes to observe data that changes a few times
// a day. Drives/Charges count only CLOSED rows so they tick exactly
// when new history becomes available.
func TestAPIMetaReportsChangeCounters(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	vehicleID, _ := store.UpsertVehicle(storage.VehicleMeta{VIN: "VIN-META", DisplayName: "Car"})
	now := time.Now().UTC()

	read := func() (drives, charges int, posID int64) {
		srv := New(store, dbPath, nil, "metric", "test")
		req := httptest.NewRequest("GET", "/api/meta", nil)
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, req)
		var out struct {
			Drives           int   `json:"drives"`
			Charges          int   `json:"charges"`
			LatestPositionID int64 `json:"latest_position_id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return out.Drives, out.Charges, out.LatestPositionID
	}

	if d, c, p := read(); d != 0 || c != 0 || p != 0 {
		t.Fatalf("expected all counters zero on an empty database, got %d/%d/%d", d, c, p)
	}

	// An OPEN drive must not tick the counter - nothing new to fetch yet.
	driveID, err := store.OpenDrive(storage.DriveStart{VehicleID: vehicleID, Time: now, OdometerKm: 100, BatteryLevel: 80})
	if err != nil {
		t.Fatalf("open drive: %v", err)
	}
	for i, odo := range []float64{101, 105} {
		if err := store.AppendPosition(storage.PositionSample{
			DriveID: driveID, VehicleID: vehicleID, Time: now.Add(time.Duration(i+1) * time.Minute),
			OdometerKm: odo, ShiftState: "D",
		}); err != nil {
			t.Fatalf("append position: %v", err)
		}
	}
	d, _, posID := read()
	if d != 0 {
		t.Fatalf("an in-progress drive must not count as new history, got %d", d)
	}
	if posID == 0 {
		t.Fatalf("expected latest_position_id to move during an active drive")
	}

	// Closing it is what makes new history available.
	if err := store.CloseDrive(storage.DriveEnd{DriveID: driveID, Time: now.Add(10 * time.Minute), OdometerKm: 110, BatteryLevel: 75}); err != nil {
		t.Fatalf("close drive: %v", err)
	}
	if d, _, _ := read(); d != 1 {
		t.Fatalf("expected the closed drive to tick the counter, got %d", d)
	}
}
