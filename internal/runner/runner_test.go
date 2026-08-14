// runner_test.go is teslalog's closest thing to a real drive without a
// real car: it runs the actual daemon loop (internal/runner.Run) against
// an httptest mock of the Owner API, scripting a "Drive #1"-style trip
// (13.7 km, battery 76%->70%, matching the design doc's own worked
// example) followed by a charging session, then idling long enough to
// trigger the sleep-suspend logic — and asserts both the resulting
// SQLite rows AND that vehicle_data stops being called once suspended.
package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"teslalog/internal/config"
	"teslalog/internal/storage"
	"teslalog/internal/tesla"
)

const milesPerKm = 1 / 1.609344

func km(v float64) float64 { return v * milesPerKm } // convert a km figure into the miles the mock Owner API returns

// --- fixtures mirroring tesla.rawVehicleData's JSON shape ---

type fixtureResponse struct {
	Response fixture `json:"response"`
}

type fixture struct {
	ID            int64  `json:"id"`
	VehicleID     int64  `json:"vehicle_id"`
	VIN           string `json:"vin"`
	DisplayName   string `json:"display_name"`
	State         string `json:"state"`
	VehicleConfig struct {
		CarType     string `json:"car_type"`
		TrimBadging string `json:"trim_badging"`
	} `json:"vehicle_config"`
	DriveState struct {
		ShiftState string  `json:"shift_state"`
		Latitude   float64 `json:"latitude"`
		Longitude  float64 `json:"longitude"`
		Speed      float64 `json:"speed"`
		Heading    float64 `json:"heading"`
		Power      float64 `json:"power"`
	} `json:"drive_state"`
	ChargeState struct {
		ChargingState        string  `json:"charging_state"`
		BatteryLevel         int     `json:"battery_level"`
		BatteryRange         float64 `json:"battery_range"`
		IdealBatteryRange    float64 `json:"ideal_battery_range"`
		ChargeEnergyAdded    float64 `json:"charge_energy_added"`
		ChargerPower         float64 `json:"charger_power"`
		ChargerVoltage       float64 `json:"charger_voltage"`
		ChargerActualCurrent float64 `json:"charger_actual_current"`
	} `json:"charge_state"`
	VehicleState struct {
		Odometer       float64 `json:"odometer"`
		CarVersion     string  `json:"car_version"`
		SoftwareUpdate struct {
			Status  string `json:"status"`
			Version string `json:"version"`
		} `json:"software_update"`
	} `json:"vehicle_state"`
}

func buildScript() []fixture {
	base := func() fixture {
		var f fixture
		f.ID, f.VehicleID = 42, 42
		f.VIN = "5YJ3E1EA1PF000001"
		f.DisplayName = "Test Model 3"
		f.State = "online"
		f.VehicleConfig.CarType = "model3"
		return f
	}

	driveStart := base()
	driveStart.DriveState.ShiftState = "D"
	driveStart.DriveState.Latitude, driveStart.DriveState.Longitude = 40.0000, -74.0000
	driveStart.ChargeState.ChargingState = "Disconnected"
	driveStart.ChargeState.BatteryLevel = 76
	driveStart.ChargeState.BatteryRange = km(300)
	driveStart.VehicleState.Odometer = km(1000.0)

	driveMid := driveStart
	driveMid.DriveState.Latitude, driveMid.DriveState.Longitude = 40.0500, -74.0400
	driveMid.ChargeState.BatteryLevel = 73
	driveMid.VehicleState.Odometer = km(1007.0)

	driveEnd := driveStart
	driveEnd.DriveState.ShiftState = "" // parked
	driveEnd.DriveState.Latitude, driveEnd.DriveState.Longitude = 40.0900, -74.0800
	driveEnd.ChargeState.BatteryLevel = 70
	driveEnd.VehicleState.Odometer = km(1013.7) // 13.7 km drive, matching the design doc's example

	chargeStart := driveEnd
	chargeStart.ChargeState.ChargingState = "Charging"
	chargeStart.ChargeState.BatteryLevel = 21 // a separate later charge in the same DB, battery level unrelated to the drive
	chargeStart.ChargeState.BatteryRange = km(84)
	chargeStart.ChargeState.ChargeEnergyAdded = 0

	chargeMid := chargeStart
	chargeMid.ChargeState.BatteryLevel = 50
	chargeMid.ChargeState.ChargeEnergyAdded = 17.4
	chargeMid.ChargeState.ChargerPower = 11.0

	chargeDone := chargeStart
	chargeDone.ChargeState.ChargingState = "Complete"
	chargeDone.ChargeState.BatteryLevel = 80
	chargeDone.ChargeState.BatteryRange = km(320)
	chargeDone.ChargeState.ChargeEnergyAdded = 34.8

	return []fixture{driveStart, driveMid, driveEnd, chargeStart, chargeMid, chargeDone}
}

func TestFullDriveAndChargeLifecycle(t *testing.T) {
	script := buildScript()
	var vehicleDataCalls int64
	var summaryCalls int64

	mux := http.NewServeMux()
	mux.HandleFunc("/api/1/vehicles", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&summaryCalls, 1)
		dataCalls := atomic.LoadInt64(&vehicleDataCalls)

		// First check: asleep (proves the initial ASLEEP path). Once
		// the drive+charge script has fully played out (and is just
		// repeating its last, idle entry), report asleep again to
		// simulate the car actually falling asleep - this must NOT
		// cause more vehicle_data calls once we're Suspended.
		state := "online"
		if n == 1 || int(dataCalls) >= len(script) {
			state = "asleep"
		}

		json.NewEncoder(w).Encode(map[string]any{
			"response": []map[string]any{{
				"id": 42, "vehicle_id": 42, "vin": "5YJ3E1EA1PF000001",
				"display_name": "Test Model 3", "state": state, "in_service": false,
			}},
			"count": 1,
		})
	})
	mux.HandleFunc("/api/1/vehicles/42/vehicle_data", func(w http.ResponseWriter, r *http.Request) {
		idx := int(atomic.AddInt64(&vehicleDataCalls, 1)) - 1
		if idx >= len(script) {
			idx = len(script) - 1
		}
		json.NewEncoder(w).Encode(fixtureResponse{Response: script[idx]})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tesla.db")
	tokenPath := filepath.Join(dir, "tokens.json")
	if err := tesla.SaveTokenFile(tokenPath, tesla.TokenSet{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("seed token file: %v", err)
	}

	cfg := config.Config{
		Database:  dbPath,
		TokenFile: tokenPath,
		Polling: config.PollingConfig{
			ActiveInterval:         2 * time.Millisecond,
			IdleTimeout:            40 * time.Millisecond,
			SuspendedCheckInterval: 2 * time.Millisecond,
		},
		Streaming: config.StreamingConfig{Enabled: false},
		Backup:    config.BackupConfig{Enabled: false},
		API: config.APIConfig{
			OwnerAPIBaseURL: server.URL,
			SSOBaseURL:      server.URL, // unused: token never expires in this test
			ClientID:        "ownerapi",
			UserAgent:       "teslalog-test/0.1",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- Run(ctx, cfg) }()

	// Poll the DB (a second connection to the same file) until the
	// state machine has recorded a 'suspended' state, proving the
	// idle-timeout-to-sleep logic actually ran end-to-end.
	deadline := time.Now().Add(8 * time.Second)
	var suspendedSeen bool
	for time.Now().Before(deadline) {
		if hasState(t, dbPath, "suspended") {
			suspendedSeen = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !suspendedSeen {
		t.Fatalf("vehicle never reached 'suspended' state within the deadline")
	}

	callsAtSuspend := atomic.LoadInt64(&vehicleDataCalls)
	time.Sleep(150 * time.Millisecond) // several suspended_check_interval ticks
	callsAfterWait := atomic.LoadInt64(&vehicleDataCalls)
	if callsAfterWait != callsAtSuspend {
		t.Fatalf("vehicle_data was called %d more time(s) after suspending - the daemon must not poll a sleeping car",
			callsAfterWait-callsAtSuspend)
	}

	cancel()
	<-runErrCh

	// --- assertions against the persisted data ---
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store for assertions: %v", err)
	}
	defer store.Close()

	var vehicleID int64
	if err := store.DB().QueryRow(`SELECT id FROM vehicles ORDER BY id LIMIT 1`).Scan(&vehicleID); err != nil {
		t.Fatalf("query vehicle id: %v", err)
	}

	drives, err := store.ListDrives(vehicleID, 0)
	if err != nil {
		t.Fatalf("list drives: %v", err)
	}
	if len(drives) != 1 {
		t.Fatalf("expected exactly 1 closed drive, got %d", len(drives))
	}
	d := drives[0]
	if d.StartBattery != 76 || d.EndBattery != 70 {
		t.Fatalf("expected battery 76%%->70%% (matching the design doc's Drive #1 example), got %d%%->%d%%", d.StartBattery, d.EndBattery)
	}
	if diff := d.DistanceKm - 13.7; diff > 0.05 || diff < -0.05 {
		t.Fatalf("expected ~13.7 km distance (matching the design doc's Drive #1 example), got %.2f km", d.DistanceKm)
	}

	var positionCount int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM positions WHERE drive_id = ?`, d.ID).Scan(&positionCount); err != nil {
		t.Fatalf("count positions: %v", err)
	}
	if positionCount == 0 {
		t.Fatalf("expected at least one GPS position recorded for the drive")
	}

	charges, err := store.ListCharges(vehicleID, 0)
	if err != nil {
		t.Fatalf("list charges: %v", err)
	}
	if len(charges) != 1 {
		t.Fatalf("expected exactly 1 closed charging session, got %d", len(charges))
	}
	c := charges[0]
	if c.StartBattery != 21 || c.EndBattery != 80 {
		t.Fatalf("expected charge 21%%->80%%, got %d%%->%d%%", c.StartBattery, c.EndBattery)
	}
	if diff := c.EnergyAddedKwh - 34.8; diff > 0.01 || diff < -0.01 {
		t.Fatalf("expected 34.8 kWh added, got %.2f", c.EnergyAddedKwh)
	}

	t.Logf("OK: drive %.1f km (%d%%->%d%%), charge %d%%->%d%% (%.1f kWh), %d vehicle_data calls total, suspended after idle timeout",
		d.DistanceKm, d.StartBattery, d.EndBattery, c.StartBattery, c.EndBattery, c.EnergyAddedKwh, atomic.LoadInt64(&vehicleDataCalls))
}

func hasState(t *testing.T, dbPath, state string) bool {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=2000")
	if err != nil {
		return false
	}
	defer db.Close()
	var count int
	// Ignore errors: the table may not exist yet on the very first poll.
	_ = db.QueryRow(`SELECT COUNT(*) FROM states WHERE state = ?`, state).Scan(&count)
	return count > 0
}

func init() {
	// Ensure a clear failure message rather than a hang if buildScript's
	// fixtures ever get out of sync with tesla's rawVehicleData shape.
	if len(buildScript()) == 0 {
		panic(fmt.Sprintf("buildScript produced no fixtures"))
	}
}
