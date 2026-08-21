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
	"log/slog"
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
		f.ID, f.VehicleID = 555, 42 // deliberately distinct: catches id/vehicle_id mixups (see VehicleSummary doc comment)
		f.VIN = "5YJ3E1EA1PF000001"
		f.DisplayName = "Test Model 3"
		f.State = "online"
		f.VehicleConfig.CarType = "model3"
		f.VehicleState.CarVersion = "2026.20.1"
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
	mux.HandleFunc("/api/1/products", func(w http.ResponseWriter, r *http.Request) {
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
				"id": 555, "vehicle_id": 42, "vin": "5YJ3E1EA1PF000001",
				"display_name": "Test Model 3", "state": state, "in_service": false,
			}},
			"count": 1,
		})
	})
	mux.HandleFunc("/api/1/vehicles/555/vehicle_data", func(w http.ResponseWriter, r *http.Request) {
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
			DrivingInterval:        2 * time.Millisecond,
			ChargingInterval:       2 * time.Millisecond,
			OnlineInterval:         2 * time.Millisecond,
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

	var firmware sql.NullString
	if err := store.DB().QueryRow(`SELECT firmware_version FROM vehicles WHERE id = ?`, vehicleID).Scan(&firmware); err != nil {
		t.Fatalf("query firmware_version: %v", err)
	}
	if firmware.String != "2026.20.1" {
		t.Fatalf("expected firmware_version %q recorded from an idle poll's car_version, got %q", "2026.20.1", firmware.String)
	}

	lifetime, err := store.Lifetime(vehicleID)
	if err != nil {
		t.Fatalf("lifetime stats: %v", err)
	}
	if diff := lifetime.OdometerKm - 1013.7; diff > 0.05 || diff < -0.05 {
		t.Fatalf("expected lifetime odometer ~1013.7 km (the drive's end odometer), got %.2f", lifetime.OdometerKm)
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

// TestRestartRecoversOpenDriveInsteadOfDuplicating simulates the daemon
// being killed (crash, OOM, systemd Restart=always) while a drive is open,
// then restarted against the same database. It must resume writing to the
// same drive row rather than leaving it open forever while starting a
// second, parallel one — see vehicle.Resume and its call site in Run.
func TestRestartRecoversOpenDriveInsteadOfDuplicating(t *testing.T) {
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

	const vin = "5YJ3E1EA1PF000001"
	summaryHandler := func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"response": []map[string]any{{
				"id": 555, "vehicle_id": 42, "vin": vin,
				"display_name": "Test Model 3", "state": "online", "in_service": false,
			}},
			"count": 1,
		})
	}

	cfgFor := func(serverURL string) config.Config {
		return config.Config{
			Database:  dbPath,
			TokenFile: tokenPath,
			Polling: config.PollingConfig{
				DrivingInterval:        2 * time.Millisecond,
				ChargingInterval:       2 * time.Millisecond,
				OnlineInterval:         2 * time.Millisecond,
				IdleTimeout:            time.Hour, // don't let idle-suspend interfere with this test
				SuspendedCheckInterval: 2 * time.Millisecond,
			},
			Streaming: config.StreamingConfig{Enabled: false},
			Backup:    config.BackupConfig{Enabled: false},
			API: config.APIConfig{
				OwnerAPIBaseURL: serverURL, SSOBaseURL: serverURL,
				ClientID: "ownerapi", UserAgent: "teslalog-test/0.1",
			},
		}
	}

	drivingFixture := func() fixture {
		var f fixture
		f.ID, f.VehicleID = 555, 42 // deliberately distinct: catches id/vehicle_id mixups (see VehicleSummary doc comment)
		f.VIN, f.DisplayName, f.State = vin, "Test Model 3", "online"
		f.VehicleConfig.CarType = "model3"
		f.DriveState.ShiftState = "D"
		f.ChargeState.ChargingState = "Disconnected"
		f.ChargeState.BatteryLevel = 76
		f.ChargeState.BatteryRange = km(300)
		f.VehicleState.Odometer = km(1000.0)
		return f
	}

	// --- Phase 1: daemon runs, opens a drive, then gets killed mid-drive ---
	mux1 := http.NewServeMux()
	mux1.HandleFunc("/api/1/products", summaryHandler)
	mux1.HandleFunc("/api/1/vehicles/555/vehicle_data", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(fixtureResponse{Response: drivingFixture()})
	})
	server1 := httptest.NewServer(mux1)
	defer server1.Close()

	ctx1, cancel1 := context.WithCancel(context.Background())
	runErrCh1 := make(chan error, 1)
	go func() { runErrCh1 <- Run(ctx1, cfgFor(server1.URL)) }()

	// Wait for a drive to actually be open (not just a fixed sleep) before
	// "crashing" it, so this isn't flaky under slow/cold-start conditions
	// (e.g. first SQLite-via-wazero open in the process).
	deadline1 := time.Now().Add(8 * time.Second)
	var driveOpened bool
	for time.Now().Before(deadline1) {
		if hasState(t, dbPath, "driving") {
			driveOpened = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel1()
	<-runErrCh1
	if !driveOpened {
		t.Fatalf("drive never opened in phase 1 within the deadline")
	}

	func() {
		store, err := storage.Open(dbPath)
		if err != nil {
			t.Fatalf("reopen store after phase 1: %v", err)
		}
		defer store.Close()
		var vehicleID int64
		if err := store.DB().QueryRow(`SELECT id FROM vehicles ORDER BY id LIMIT 1`).Scan(&vehicleID); err != nil {
			t.Fatalf("query vehicle id: %v", err)
		}
		openID, err := store.OpenDriveID(vehicleID)
		if err != nil {
			t.Fatalf("query open drive: %v", err)
		}
		if openID == 0 {
			t.Fatalf("expected a drive left open after phase 1, found none")
		}
		var count int
		if err := store.DB().QueryRow(`SELECT COUNT(*) FROM drives`).Scan(&count); err != nil {
			t.Fatalf("count drives: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 drive row after phase 1, got %d", count)
		}
	}()

	// --- Phase 2: daemon restarts against the same DB; car is now parked ---
	parkedFixture := drivingFixture()
	parkedFixture.DriveState.ShiftState = ""
	parkedFixture.ChargeState.BatteryLevel = 70
	parkedFixture.VehicleState.Odometer = km(1013.7)

	mux2 := http.NewServeMux()
	mux2.HandleFunc("/api/1/products", summaryHandler)
	mux2.HandleFunc("/api/1/vehicles/555/vehicle_data", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(fixtureResponse{Response: parkedFixture})
	})
	server2 := httptest.NewServer(mux2)
	defer server2.Close()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- Run(ctx2, cfgFor(server2.URL)) }()

	deadline := time.Now().Add(8 * time.Second)
	var closed bool
	for time.Now().Before(deadline) {
		if hasState(t, dbPath, "idle") {
			closed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel2()
	<-runErrCh
	if !closed {
		t.Fatalf("vehicle never settled back into 'idle' after restart")
	}

	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store after phase 2: %v", err)
	}
	defer store.Close()

	var count int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM drives`).Scan(&count); err != nil {
		t.Fatalf("count drives: %v", err)
	}
	if count != 1 {
		t.Fatalf("restart must resume the same drive, not open a second one; got %d drive rows", count)
	}

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
		t.Fatalf("expected the resumed drive to carry its original start (76%%) through to the post-restart end (70%%), got %d%%->%d%%",
			d.StartBattery, d.EndBattery)
	}
}

// TestWakingUpDoesNotWaitOutTheFullSuspendedInterval pins a real bug found
// against a live car: once the cheap summary check discovers the vehicle
// went from asleep/suspended to active on its own, the daemon must start
// polling vehicle_data on the very next loop iteration, not wait out the
// (typically much longer, e.g. 15 minutes) SuspendedCheckInterval first.
// The bug was that `sleepFor` got set to SuspendedCheckInterval
// unconditionally inside the asleep-like branch, even when checkSummary
// had just transitioned the state out of it in that same iteration.
func TestWakingUpDoesNotWaitOutTheFullSuspendedInterval(t *testing.T) {
	const vin = "5YJ3E1EA1PF000001"
	var summaryCalls, vehicleDataCalls int64

	mux := http.NewServeMux()
	mux.HandleFunc("/api/1/products", func(w http.ResponseWriter, r *http.Request) {
		// First two checks: asleep - one from Run's own pickVehicle, one
		// from run()'s pre-loop checkSummary, so the machine is actually
		// Asleep (not Online already) by the time the for loop's first
		// iteration runs its own checkSummary in the asleep-like branch,
		// which is the transition this test is actually exercising.
		// Every check after that: online.
		state := "online"
		if atomic.AddInt64(&summaryCalls, 1) <= 2 {
			state = "asleep"
		}
		json.NewEncoder(w).Encode(map[string]any{
			"response": []map[string]any{{
				"id": 555, "vehicle_id": 42, "vin": vin,
				"display_name": "Test Model 3", "state": state, "in_service": false,
			}},
			"count": 1,
		})
	})
	mux.HandleFunc("/api/1/vehicles/555/vehicle_data", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&vehicleDataCalls, 1)
		f := fixture{ID: 555, VehicleID: 42, VIN: vin, DisplayName: "Test Model 3", State: "online"}
		f.ChargeState.BatteryLevel = 70
		json.NewEncoder(w).Encode(fixtureResponse{Response: f})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tesla.db")
	tokenPath := filepath.Join(dir, "tokens.json")
	if err := tesla.SaveTokenFile(tokenPath, tesla.TokenSet{
		AccessToken: "test-access-token", RefreshToken: "test-refresh-token",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("seed token file: %v", err)
	}

	cfg := config.Config{
		Database:  dbPath,
		TokenFile: tokenPath,
		Polling: config.PollingConfig{
			DrivingInterval: 2 * time.Millisecond, ChargingInterval: 2 * time.Millisecond,
			OnlineInterval: 2 * time.Millisecond, IdleTimeout: time.Hour,
			// Deliberately long relative to the test's own deadline below:
			// if the bug is present, vehicle_data will never be called
			// before this elapses and the test will time out waiting.
			SuspendedCheckInterval: 5 * time.Second,
		},
		Streaming: config.StreamingConfig{Enabled: false},
		Backup:    config.BackupConfig{Enabled: false},
		API: config.APIConfig{
			OwnerAPIBaseURL: server.URL, SSOBaseURL: server.URL,
			ClientID: "ownerapi", UserAgent: "teslalog-test/0.1",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- Run(ctx, cfg) }()

	deadline := time.Now().Add(2 * time.Second) // well under SuspendedCheckInterval
	var polled bool
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&vehicleDataCalls) > 0 {
			polled = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-runErrCh

	if !polled {
		t.Fatalf("vehicle_data was never called within 2s of waking up - the daemon waited out the full %s SuspendedCheckInterval instead of resuming fast polling immediately",
			cfg.Polling.SuspendedCheckInterval)
	}
}

// TestTeeHandlerForwardsToBothHandlers pins the portal's log-capture
// mechanism: enabling the portal must tee log output into its in-memory
// buffer WITHOUT silencing or altering what still goes to the normal
// handler (stderr/journalctl) - neither destination should ever lose
// records because of the other.
func TestTeeHandlerForwardsToBothHandlers(t *testing.T) {
	var aRecords, bRecords []string
	a := &recordingHandler{out: &aRecords}
	b := &recordingHandler{out: &bRecords}

	logger := slog.New(teeHandler{a, b})
	logger.Info("hello", "x", 1)
	logger.With("component", "test").Warn("world")

	if len(aRecords) != 2 || len(bRecords) != 2 {
		t.Fatalf("expected both handlers to receive both records, got a=%v b=%v", aRecords, bRecords)
	}
	if aRecords[0] != "INFO hello x=1" || bRecords[0] != "INFO hello x=1" {
		t.Fatalf("unexpected first record: a=%q b=%q", aRecords[0], bRecords[0])
	}
	if aRecords[1] != "WARN world component=test" || bRecords[1] != "WARN world component=test" {
		t.Fatalf("unexpected second record (WithAttrs not forwarded correctly?): a=%q b=%q", aRecords[1], bRecords[1])
	}
}

// recordingHandler is a minimal slog.Handler recording "LEVEL message
// key=value..." lines, used only to verify teeHandler forwards
// everything (message, level, and .With attrs) to each inner handler.
type recordingHandler struct {
	out   *[]string
	attrs []slog.Attr
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	line := r.Level.String() + " " + r.Message
	for _, a := range h.attrs {
		line += " " + a.Key + "=" + a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()
		return true
	})
	*h.out = append(*h.out, line)
	return nil
}

func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordingHandler{out: h.out, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

// TestDriveAndChargeLocationsResolveViaGeofence proves the geofencing
// wiring end-to-end: a drive/charge whose start/end coordinates fall
// inside a configured [[geofence]] must come out of the database with
// that geofence's name in start_location/end_location/location, not
// just resolvable in isolation (internal/geocode has its own unit
// tests for the resolution logic itself).
func TestDriveAndChargeLocationsResolveViaGeofence(t *testing.T) {
	const vin = "5YJ3E1EA1PF000001"
	homeLat, homeLng := 40.0000, -74.0000 // matches driveStart/chargeStart below
	workLat, workLng := 40.0900, -74.0800 // matches driveEnd below

	fx := func(lat, lng float64, shiftState, chargingState string, battery int, odometerKm float64) fixture {
		var f fixture
		f.ID, f.VehicleID = 555, 42
		f.VIN, f.DisplayName, f.State = vin, "Test Model 3", "online"
		f.VehicleConfig.CarType = "model3"
		f.DriveState.ShiftState = shiftState
		f.DriveState.Latitude, f.DriveState.Longitude = lat, lng
		f.ChargeState.ChargingState = chargingState
		f.ChargeState.BatteryLevel = battery
		f.VehicleState.Odometer = km(odometerKm)
		return f
	}
	script := []fixture{
		fx(homeLat, homeLng, "D", "Disconnected", 76, 1000), // drive start, at "Home"
		fx(workLat, workLng, "", "Disconnected", 70, 1010),  // drive end, at "Work"
		fx(homeLat, homeLng, "", "Charging", 70, 1010),      // charge start, back at "Home"
	}

	var vehicleDataCalls int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/1/products", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"response": []map[string]any{{
				"id": 555, "vehicle_id": 42, "vin": vin,
				"display_name": "Test Model 3", "state": "online", "in_service": false,
			}},
			"count": 1,
		})
	})
	mux.HandleFunc("/api/1/vehicles/555/vehicle_data", func(w http.ResponseWriter, r *http.Request) {
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
		AccessToken: "test-access-token", RefreshToken: "test-refresh-token",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("seed token file: %v", err)
	}

	cfg := config.Config{
		Database:  dbPath,
		TokenFile: tokenPath,
		Polling: config.PollingConfig{
			DrivingInterval: 2 * time.Millisecond, ChargingInterval: 2 * time.Millisecond,
			OnlineInterval: 2 * time.Millisecond, IdleTimeout: time.Hour,
			SuspendedCheckInterval: 2 * time.Millisecond,
		},
		Streaming: config.StreamingConfig{Enabled: false},
		Backup:    config.BackupConfig{Enabled: false},
		Geofences: []config.GeofenceConfig{
			{Name: "Home", Lat: homeLat, Lng: homeLng, RadiusM: 50},
			{Name: "Work", Lat: workLat, Lng: workLng, RadiusM: 50},
		},
		API: config.APIConfig{
			OwnerAPIBaseURL: server.URL, SSOBaseURL: server.URL,
			ClientID: "ownerapi", UserAgent: "teslalog-test/0.1",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- Run(ctx, cfg) }()

	deadline := time.Now().Add(8 * time.Second)
	var chargeSeen bool
	for time.Now().Before(deadline) {
		if hasState(t, dbPath, "charging") {
			chargeSeen = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-runErrCh
	if !chargeSeen {
		t.Fatalf("never reached 'charging' state within the deadline")
	}

	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()

	var startLoc, endLoc sql.NullString
	if err := store.DB().QueryRow(`SELECT start_location, end_location FROM drives ORDER BY id LIMIT 1`).Scan(&startLoc, &endLoc); err != nil {
		t.Fatalf("query drive locations: %v", err)
	}
	if startLoc.String != "Home" {
		t.Fatalf("expected drive start_location 'Home', got %q", startLoc.String)
	}
	if endLoc.String != "Work" {
		t.Fatalf("expected drive end_location 'Work', got %q", endLoc.String)
	}

	var chargeLoc sql.NullString
	if err := store.DB().QueryRow(`SELECT location FROM charging_sessions ORDER BY id LIMIT 1`).Scan(&chargeLoc); err != nil {
		t.Fatalf("query charge location: %v", err)
	}
	if chargeLoc.String != "Home" {
		t.Fatalf("expected charging_sessions.location 'Home', got %q", chargeLoc.String)
	}
}
