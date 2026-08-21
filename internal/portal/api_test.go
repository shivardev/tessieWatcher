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

	srv := New(store, dbPath, nil, "metric")
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
	srv := New(store, "unused.db", nil, "metric")

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

	srv := New(store, dbPath, nil, "metric")
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
	srv := New(store, "unused.db", nil, "metric")

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
	srv := New(store, "unused.db", nil, "metric")

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
