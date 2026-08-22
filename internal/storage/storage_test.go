package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertVehicle(t *testing.T) {
	s := openTestStore(t)

	id1, err := s.UpsertVehicle(VehicleMeta{VIN: "5YJ3E1EA1PF000001", TeslaID: "123", DisplayName: "My Model 3", Model: "3", TrimBadging: "74D", MarketingName: "LR AWD"})
	if err != nil {
		t.Fatalf("upsert vehicle: %v", err)
	}
	if id1 == 0 {
		t.Fatalf("expected nonzero id")
	}

	// Upserting again with the same VIN should return the same id, and
	// update the mutable fields.
	id2, err := s.UpsertVehicle(VehicleMeta{VIN: "5YJ3E1EA1PF000001", TeslaID: "123", DisplayName: "Renamed", Model: "3", TrimBadging: "74D", MarketingName: "LR AWD"})
	if err != nil {
		t.Fatalf("re-upsert vehicle: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("expected same id on upsert, got %d and %d", id1, id2)
	}

	var marketingName string
	if err := s.db.QueryRow(`SELECT marketing_name FROM vehicles WHERE id = ?`, id1).Scan(&marketingName); err != nil {
		t.Fatalf("query marketing_name: %v", err)
	}
	if marketingName != "LR AWD" {
		t.Fatalf("expected marketing_name %q, got %q", "LR AWD", marketingName)
	}

	var name string
	if err := s.db.QueryRow(`SELECT display_name FROM vehicles WHERE id = ?`, id1).Scan(&name); err != nil {
		t.Fatalf("query display_name: %v", err)
	}
	if name != "Renamed" {
		t.Fatalf("expected updated display_name, got %q", name)
	}
}

func TestDriveRoundTrip(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN1", TeslaID: "1", DisplayName: "Car", Model: "model3", TrimBadging: ""})

	start := time.Date(2026, 8, 14, 10, 42, 0, 0, time.UTC)
	driveID, err := s.OpenDrive(DriveStart{
		VehicleID: vehicleID, Time: start, OdometerKm: 1000, BatteryLevel: 76, RangeKm: 300, Lat: 40.0, Lng: -74.0,
	})
	if err != nil {
		t.Fatalf("open drive: %v", err)
	}

	for i := 0; i < 5; i++ {
		err := s.AppendPosition(PositionSample{
			DriveID: driveID, VehicleID: vehicleID,
			Time:         start.Add(time.Duration(i) * time.Minute),
			Lat:          40.0 + float64(i)*0.01,
			Lng:          -74.0 - float64(i)*0.01,
			SpeedKmh:     50 + float64(i),
			OdometerKm:   1000 + float64(i),
			BatteryLevel: 76 - i,
			ShiftState:   "D",
		})
		if err != nil {
			t.Fatalf("append position %d: %v", i, err)
		}
	}

	end := start.Add(21 * time.Minute)
	if err := s.CloseDrive(DriveEnd{
		DriveID: driveID, Time: end, OdometerKm: 1013.7, BatteryLevel: 70, RangeKm: 282, Lat: 40.1, Lng: -74.2,
	}); err != nil {
		t.Fatalf("close drive: %v", err)
	}

	drives, err := s.ListDrives(vehicleID, 0)
	if err != nil {
		t.Fatalf("list drives: %v", err)
	}
	if len(drives) != 1 {
		t.Fatalf("expected 1 drive, got %d", len(drives))
	}
	d := drives[0]
	if d.StartBattery != 76 || d.EndBattery != 70 {
		t.Fatalf("unexpected battery levels: %+v", d)
	}
	wantDistance := 13.7
	if diff := d.DistanceKm - wantDistance; diff > 0.01 || diff < -0.01 {
		t.Fatalf("expected distance ~%.1f km, got %.2f", wantDistance, d.DistanceKm)
	}
	if diff := d.DurationMin - 21.0; diff > 0.01 || diff < -0.01 {
		t.Fatalf("expected duration 21 min, got %.2f", d.DurationMin)
	}

	// Drive should no longer be "open".
	openID, err := s.OpenDriveID(vehicleID)
	if err != nil {
		t.Fatalf("open drive id: %v", err)
	}
	if openID != 0 {
		t.Fatalf("expected no open drive, got id %d", openID)
	}
}

// TestCloseDriveFromLastPositionUsesLastRecordedSample is a regression
// test for a real, live-found bug (see vehicle.Machine.OnUnreachable's
// doc comment): when the vehicle stops reporting entirely mid-drive,
// there's no fresh telemetry to close the drive with - this closes it
// using whatever position was last actually recorded instead of
// leaving it open forever (TeslaMate's own real history shows this
// exact failure mode with no auto-recovery - "Incomplete Drives").
func TestCloseDriveFromLastPositionUsesLastRecordedSample(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN-ABANDON", TeslaID: "8", DisplayName: "Car"})

	start := time.Date(2026, 8, 21, 23, 9, 0, 0, time.UTC)
	driveID, err := s.OpenDrive(DriveStart{VehicleID: vehicleID, Time: start, OdometerKm: 1000, BatteryLevel: 80, RangeKm: 300, Lat: 35.0, Lng: -85.0})
	if err != nil {
		t.Fatalf("open drive: %v", err)
	}

	if err := s.AppendPosition(PositionSample{
		DriveID: driveID, VehicleID: vehicleID, Time: start.Add(time.Minute),
		Lat: 35.02, Lng: -85.02, SpeedKmh: 60, OdometerKm: 1003, BatteryLevel: 78, RangeKm: 296, ShiftState: "D",
	}); err != nil {
		t.Fatalf("append position 1: %v", err)
	}
	lastKnown := start.Add(5 * time.Minute)
	if err := s.AppendPosition(PositionSample{
		DriveID: driveID, VehicleID: vehicleID, Time: lastKnown,
		Lat: 35.05, Lng: -85.05, SpeedKmh: 60, OdometerKm: 1008, BatteryLevel: 74, RangeKm: 288, ShiftState: "D",
	}); err != nil {
		t.Fatalf("append position 2: %v", err)
	}

	// The vehicle goes unreachable here - detected/closed much later
	// (simulating hours offline), but the drive's end_time should
	// reflect the LAST REAL SAMPLE (lastKnown), not the detection time.
	detectedAt := lastKnown.Add(3 * time.Hour)
	if err := s.CloseDriveFromLastPosition(driveID, detectedAt); err != nil {
		t.Fatalf("close abandoned drive: %v", err)
	}

	var endTime string
	var endOdometer, endRange float64
	var endBattery int
	var status string
	if err := s.db.QueryRow(`SELECT end_time, end_odometer_km, end_battery_level, end_range_km, status FROM drives WHERE id = ?`, driveID).
		Scan(&endTime, &endOdometer, &endBattery, &endRange, &status); err != nil {
		t.Fatalf("query closed drive: %v", err)
	}
	if status != "closed" {
		t.Fatalf("expected status closed, got %q", status)
	}
	if endOdometer != 1008 || endBattery != 74 || endRange != 288 {
		t.Fatalf("expected end values from the last recorded position (odo=1008 batt=74 range=288), got odo=%v batt=%v range=%v", endOdometer, endBattery, endRange)
	}
	gotEnd, err := time.Parse(timeLayout, endTime)
	if err != nil {
		t.Fatalf("parse end_time: %v", err)
	}
	if !gotEnd.Equal(lastKnown) {
		t.Fatalf("expected end_time to be the last recorded position's timestamp (%s), not the detection time - got %s", lastKnown, gotEnd)
	}
}

func TestCloseDriveFromLastPositionWithNoPositionsDiscardsTheRow(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN-ABANDON-2", TeslaID: "9", DisplayName: "Car"})
	start := time.Now().UTC()
	driveID, err := s.OpenDrive(DriveStart{VehicleID: vehicleID, Time: start, OdometerKm: 1000, BatteryLevel: 80})
	if err != nil {
		t.Fatalf("open drive: %v", err)
	}

	// No positions ever recorded (the vehicle went unreachable before
	// a single sample landed) - matches TeslaMate's own close_drive
	// validity check (count >= 2, verified directly against its
	// source): discarded outright rather than kept as a permanent,
	// meaningless row with every end_* field NULL.
	if err := s.CloseDriveFromLastPosition(driveID, start.Add(time.Hour)); err != nil {
		t.Fatalf("close abandoned drive with no positions: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM drives WHERE id = ?`, driveID).Scan(&count); err != nil {
		t.Fatalf("query drive: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the drive to be discarded (no positions ever recorded), but it still exists")
	}
}

// TestCloseDriveDiscardsTrivialDrives is a regression test for a real
// gap: teslalog's very first summarized read of TeslaMate's source
// claimed "no minimum-duration/distance filtering exists" - false.
// TeslaMate's real close_drive (verified directly against its source,
// lib/teslamate/log.ex) discards any drive with fewer than 2 positions
// or less than 0.01 km of distance (a bumped shifter, GPS jitter
// briefly registering as movement) rather than keeping it as a
// permanent, meaningless row.
func TestCloseDriveDiscardsTrivialDrives(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN-TRIVIAL", TeslaID: "10", DisplayName: "Car"})
	start := time.Now().UTC()

	t.Run("only one position ever recorded", func(t *testing.T) {
		driveID, err := s.OpenDrive(DriveStart{VehicleID: vehicleID, Time: start, OdometerKm: 1000, BatteryLevel: 80})
		if err != nil {
			t.Fatalf("open drive: %v", err)
		}
		if err := s.AppendPosition(PositionSample{DriveID: driveID, VehicleID: vehicleID, Time: start, OdometerKm: 1000.005, ShiftState: "D"}); err != nil {
			t.Fatalf("append position: %v", err)
		}
		if err := s.CloseDrive(DriveEnd{DriveID: driveID, Time: start.Add(time.Second), OdometerKm: 1000.005}); err != nil {
			t.Fatalf("close drive: %v", err)
		}
		var count int
		s.db.QueryRow(`SELECT COUNT(*) FROM drives WHERE id = ?`, driveID).Scan(&count)
		if count != 0 {
			t.Fatalf("expected a single-position drive to be discarded")
		}
	})

	t.Run("two positions but under the distance threshold", func(t *testing.T) {
		driveID, err := s.OpenDrive(DriveStart{VehicleID: vehicleID, Time: start, OdometerKm: 2000, BatteryLevel: 80})
		if err != nil {
			t.Fatalf("open drive: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := s.AppendPosition(PositionSample{DriveID: driveID, VehicleID: vehicleID, Time: start.Add(time.Duration(i) * time.Second), OdometerKm: 2000.001, ShiftState: "D"}); err != nil {
				t.Fatalf("append position %d: %v", i, err)
			}
		}
		// 0.001 km moved - well under the 0.01 km threshold.
		if err := s.CloseDrive(DriveEnd{DriveID: driveID, Time: start.Add(2 * time.Second), OdometerKm: 2000.001}); err != nil {
			t.Fatalf("close drive: %v", err)
		}
		var count int
		s.db.QueryRow(`SELECT COUNT(*) FROM drives WHERE id = ?`, driveID).Scan(&count)
		if count != 0 {
			t.Fatalf("expected a sub-10-meter drive to be discarded")
		}
	})

	t.Run("a real drive is kept", func(t *testing.T) {
		driveID, err := s.OpenDrive(DriveStart{VehicleID: vehicleID, Time: start, OdometerKm: 3000, BatteryLevel: 80})
		if err != nil {
			t.Fatalf("open drive: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := s.AppendPosition(PositionSample{DriveID: driveID, VehicleID: vehicleID, Time: start.Add(time.Duration(i) * time.Minute), OdometerKm: 3000 + float64(i), ShiftState: "D"}); err != nil {
				t.Fatalf("append position %d: %v", i, err)
			}
		}
		if err := s.CloseDrive(DriveEnd{DriveID: driveID, Time: start.Add(3 * time.Minute), OdometerKm: 3002}); err != nil {
			t.Fatalf("close drive: %v", err)
		}
		var count int
		var status string
		s.db.QueryRow(`SELECT COUNT(*), status FROM drives WHERE id = ?`, driveID).Scan(&count, &status)
		if count != 1 || status != "closed" {
			t.Fatalf("expected a real 2km drive to be kept and closed, got count=%d status=%q", count, status)
		}
	})
}

// TestCloseChargingSessionFallsBackToMaxEnergyAddedWhenLastReadIsZero
// is a regression test matching a known Tesla API quirk TeslaMate's
// own complete_charging_process explicitly guards against (verified
// directly against its source): the very last sample before a charge
// ends sometimes reports charge_energy_added as exactly 0, even though
// real energy was added throughout the session.
func TestCloseChargingSessionFallsBackToMaxEnergyAddedWhenLastReadIsZero(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN-ZEROQUIRK", TeslaID: "11", DisplayName: "Car"})
	start := time.Now().UTC()
	sessionID, err := s.OpenChargingSession(ChargeStart{VehicleID: vehicleID, Time: start, BatteryLevel: 20})
	if err != nil {
		t.Fatalf("open charging session: %v", err)
	}

	for i, added := range []float64{0, 5.2, 11.8} {
		if err := s.AppendChargingSample(ChargingSample{
			ChargingSessionID: sessionID, VehicleID: vehicleID, Time: start.Add(time.Duration(i) * time.Minute),
			BatteryLevel: 20 + i*10, EnergyAddedKwh: added,
		}); err != nil {
			t.Fatalf("append charging sample %d: %v", i, err)
		}
	}

	// The final snapshot at close time reads exactly 0 (the quirk).
	if err := s.CloseChargingSession(ChargeEnd{ChargingSessionID: sessionID, Time: start.Add(3 * time.Minute), BatteryLevel: 50, EnergyAddedKwh: 0}); err != nil {
		t.Fatalf("close charging session: %v", err)
	}

	var energyAdded float64
	if err := s.db.QueryRow(`SELECT charge_energy_added_kwh FROM charging_sessions WHERE id = ?`, sessionID).Scan(&energyAdded); err != nil {
		t.Fatalf("query charging session: %v", err)
	}
	if energyAdded != 11.8 {
		t.Fatalf("expected the max energy_added seen across samples (11.8) as a fallback for a literal-0 final reading, got %v", energyAdded)
	}
}

func TestChargingRoundTrip(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN2", TeslaID: "2", DisplayName: "Car", Model: "model3", TrimBadging: ""})

	start := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	sessionID, err := s.OpenChargingSession(ChargeStart{VehicleID: vehicleID, Time: start, BatteryLevel: 21, RangeKm: 84})
	if err != nil {
		t.Fatalf("open charging session: %v", err)
	}

	if err := s.AppendChargingSample(ChargingSample{
		ChargingSessionID: sessionID, VehicleID: vehicleID, Time: start.Add(30 * time.Minute),
		BatteryLevel: 50, ChargerPowerKw: 11, EnergyAddedKwh: 17.4, RangeKm: 200,
	}); err != nil {
		t.Fatalf("append charging sample: %v", err)
	}

	end := start.Add(time.Hour)
	if err := s.CloseChargingSession(ChargeEnd{
		ChargingSessionID: sessionID, Time: end, BatteryLevel: 80, RangeKm: 320, EnergyAddedKwh: 34.8,
	}); err != nil {
		t.Fatalf("close charging session: %v", err)
	}

	charges, err := s.ListCharges(vehicleID, 0)
	if err != nil {
		t.Fatalf("list charges: %v", err)
	}
	if len(charges) != 1 {
		t.Fatalf("expected 1 charge, got %d", len(charges))
	}
	c := charges[0]
	if c.StartBattery != 21 || c.EndBattery != 80 {
		t.Fatalf("unexpected battery levels: %+v", c)
	}
	if c.EnergyAddedKwh != 34.8 {
		t.Fatalf("expected 34.8 kWh added, got %.2f", c.EnergyAddedKwh)
	}

	openID, err := s.OpenChargingSessionID(vehicleID)
	if err != nil {
		t.Fatalf("open charging session id: %v", err)
	}
	if openID != 0 {
		t.Fatalf("expected no open charging session, got %d", openID)
	}
}

func TestStateHistory(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN3", TeslaID: "3", DisplayName: "Car", Model: "model3", TrimBadging: ""})

	t0 := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	if _, err := s.OpenState(vehicleID, "online", t0); err != nil {
		t.Fatalf("open state: %v", err)
	}
	cur, err := s.CurrentState(vehicleID)
	if err != nil || cur != "online" {
		t.Fatalf("expected current state 'online', got %q err=%v", cur, err)
	}

	if _, err := s.OpenState(vehicleID, "driving", t0.Add(time.Minute)); err != nil {
		t.Fatalf("open state 2: %v", err)
	}
	cur, err = s.CurrentState(vehicleID)
	if err != nil || cur != "driving" {
		t.Fatalf("expected current state 'driving', got %q err=%v", cur, err)
	}

	// The previous state row should now have an ended_at.
	var endedAt *string
	if err := s.db.QueryRow(`SELECT ended_at FROM states WHERE vehicle_id = ? AND state = 'online'`, vehicleID).Scan(&endedAt); err != nil {
		t.Fatalf("query ended_at: %v", err)
	}
	if endedAt == nil {
		t.Fatalf("expected previous state to have ended_at set")
	}
}

func TestBatterySamplesAndSoftwareUpdates(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN4", TeslaID: "4", DisplayName: "Car", Model: "model3", TrimBadging: ""})

	now := time.Now().UTC()
	if err := s.InsertBatterySample(vehicleID, now, 80, 320, 340, "poll"); err != nil {
		t.Fatalf("insert battery sample: %v", err)
	}

	if err := s.UpsertSoftwareUpdateStart(vehicleID, "2026.20.1", now); err != nil {
		t.Fatalf("start software update: %v", err)
	}
	// Calling it again for the same in-progress version should be a no-op, not an error.
	if err := s.UpsertSoftwareUpdateStart(vehicleID, "2026.20.1", now.Add(time.Minute)); err != nil {
		t.Fatalf("re-start software update: %v", err)
	}
	if err := s.CompleteSoftwareUpdate(vehicleID, "2026.20.1", now.Add(20*time.Minute)); err != nil {
		t.Fatalf("complete software update: %v", err)
	}

	var status string
	if err := s.db.QueryRow(`SELECT status FROM software_updates WHERE vehicle_id = ? AND version = ?`, vehicleID, "2026.20.1").Scan(&status); err != nil {
		t.Fatalf("query software update status: %v", err)
	}
	if status != "installed" {
		t.Fatalf("expected status 'installed', got %q", status)
	}
}

func TestPositionCarriesSentryValetKeeperModeFields(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN5", TeslaID: "5", DisplayName: "Car"})
	driveID, err := s.OpenDrive(DriveStart{VehicleID: vehicleID, Time: time.Now().UTC(), OdometerKm: 10})
	if err != nil {
		t.Fatalf("open drive: %v", err)
	}

	sentryOn, userPresent, valetOff := true, true, false
	if err := s.AppendPosition(PositionSample{
		DriveID: driveID, VehicleID: vehicleID, Time: time.Now().UTC(),
		SentryMode: &sentryOn, IsUserPresent: &userPresent, ValetMode: &valetOff, ClimateKeeperMode: "dog",
	}); err != nil {
		t.Fatalf("append position: %v", err)
	}

	var sentry, present, valet int
	var keeper string
	if err := s.db.QueryRow(`
		SELECT sentry_mode, is_user_present, valet_mode, climate_keeper_mode FROM positions WHERE drive_id = ?
	`, driveID).Scan(&sentry, &present, &valet, &keeper); err != nil {
		t.Fatalf("query position: %v", err)
	}
	if sentry != 1 || present != 1 || valet != 0 || keeper != "dog" {
		t.Fatalf("unexpected values: sentry=%d present=%d valet=%d keeper=%q", sentry, present, valet, keeper)
	}
}

// TestAppendPositionLeavesStreamingOnlyFieldsNullWhenOmitted is a
// regression test for a real bug found live: a streaming-derived
// position (see runner.go's drainStream) only ever sets a subset of
// PositionSample's fields - fan/climate/defroster state, TPMS, usable
// battery %, ideal/estimated range, and sentry/valet/user-present
// aren't in the streaming protocol at all. When those fields were
// plain (non-pointer) Go types, an unset field silently wrote its Go
// zero value (0, false) into a column that's perfectly capable of
// storing NULL - indistinguishable from "really is off/zero" to
// anything reading the data back, and wrong roughly 90% of the time
// (streaming reports far more frequently than the REST poll interval,
// so the large majority of any drive's positions are streaming-only).
// Every one of these fields is now a pointer - nil in, NULL out.
func TestAppendPositionLeavesStreamingOnlyFieldsNullWhenOmitted(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN-STREAM", TeslaID: "7", DisplayName: "Car"})
	driveID, err := s.OpenDrive(DriveStart{VehicleID: vehicleID, Time: time.Now().UTC(), OdometerKm: 10})
	if err != nil {
		t.Fatalf("open drive: %v", err)
	}

	// Mirrors exactly what drainStream constructs: only the fields the
	// streaming protocol actually reports.
	if err := s.AppendPosition(PositionSample{
		DriveID: driveID, VehicleID: vehicleID, Time: time.Now().UTC(),
		Lat: 40.0, Lng: -74.0, SpeedKmh: 60, Heading: 90,
		ElevationM: nil, PowerKw: 10, OdometerKm: 1000,
		BatteryLevel: 70, RangeKm: 200, ShiftState: "D",
	}); err != nil {
		t.Fatalf("append streaming-style position: %v", err)
	}

	row := s.db.QueryRow(`
		SELECT usable_battery_level, ideal_range_km, est_range_km, battery_heater_on,
		       fan_status, driver_temp_setting_c, passenger_temp_setting_c,
		       is_climate_on, is_rear_defroster_on, is_front_defroster_on,
		       tpms_pressure_fl, tpms_pressure_fr, tpms_pressure_rl, tpms_pressure_rr,
		       sentry_mode, is_user_present, valet_mode,
		       battery_level, range_km
		FROM positions WHERE drive_id = ?
	`, driveID)

	var usableBatt, idealRange, estRange, battHeater, fan, driverTemp, passengerTemp,
		isClimate, isRearDef, isFrontDef, tpmsFL, tpmsFR, tpmsRL, tpmsRR,
		sentry, userPresent, valet sql.NullString
	var battLevel int
	var rangeKm float64
	if err := row.Scan(&usableBatt, &idealRange, &estRange, &battHeater, &fan, &driverTemp, &passengerTemp,
		&isClimate, &isRearDef, &isFrontDef, &tpmsFL, &tpmsFR, &tpmsRL, &tpmsRR,
		&sentry, &userPresent, &valet, &battLevel, &rangeKm); err != nil {
		t.Fatalf("query position: %v", err)
	}

	for name, got := range map[string]sql.NullString{
		"usable_battery_level": usableBatt, "ideal_range_km": idealRange, "est_range_km": estRange,
		"battery_heater_on": battHeater, "fan_status": fan, "driver_temp_setting_c": driverTemp,
		"passenger_temp_setting_c": passengerTemp, "is_climate_on": isClimate,
		"is_rear_defroster_on": isRearDef, "is_front_defroster_on": isFrontDef,
		"tpms_pressure_fl": tpmsFL, "tpms_pressure_fr": tpmsFR, "tpms_pressure_rl": tpmsRL, "tpms_pressure_rr": tpmsRR,
		"sentry_mode": sentry, "is_user_present": userPresent, "valet_mode": valet,
	} {
		if got.Valid {
			t.Errorf("%s: expected NULL for a streaming-only sample, got %q", name, got.String)
		}
	}

	// Fields both sources report should still be the real values, not NULL.
	if battLevel != 70 || rangeKm != 200 {
		t.Fatalf("expected battery_level=70 range_km=200 (always known), got %d/%.0f", battLevel, rangeKm)
	}
}

func TestChargingSampleCarriesChargeLimitSoc(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN6", TeslaID: "6", DisplayName: "Car"})
	sessionID, err := s.OpenChargingSession(ChargeStart{VehicleID: vehicleID, Time: time.Now().UTC(), BatteryLevel: 50})
	if err != nil {
		t.Fatalf("open charging session: %v", err)
	}

	if err := s.AppendChargingSample(ChargingSample{
		ChargingSessionID: sessionID, VehicleID: vehicleID, Time: time.Now().UTC(),
		BatteryLevel: 60, ChargeLimitSoc: 80,
	}); err != nil {
		t.Fatalf("append charging sample: %v", err)
	}

	var limit int
	if err := s.db.QueryRow(`SELECT charge_limit_soc FROM charging_samples WHERE charging_session_id = ?`, sessionID).Scan(&limit); err != nil {
		t.Fatalf("query charging sample: %v", err)
	}
	if limit != 80 {
		t.Fatalf("expected charge_limit_soc 80, got %d", limit)
	}
}

func TestChargeSummaryDerivesACvsDCFromSamples(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN7", TeslaID: "7", DisplayName: "Car"})

	start := time.Now().UTC()
	dcID, err := s.OpenChargingSession(ChargeStart{VehicleID: vehicleID, Time: start, BatteryLevel: 20, RangeKm: 80})
	if err != nil {
		t.Fatalf("open dc session: %v", err)
	}
	if err := s.AppendChargingSample(ChargingSample{
		ChargingSessionID: dcID, VehicleID: vehicleID, Time: start.Add(time.Minute),
		BatteryLevel: 60, FastChargerPresent: true, RangeKm: 240,
	}); err != nil {
		t.Fatalf("append dc sample: %v", err)
	}
	if err := s.CloseChargingSession(ChargeEnd{ChargingSessionID: dcID, Time: start.Add(20 * time.Minute), BatteryLevel: 80, RangeKm: 320, EnergyAddedKwh: 40}); err != nil {
		t.Fatalf("close dc session: %v", err)
	}

	acStart := start.Add(time.Hour)
	acID, err := s.OpenChargingSession(ChargeStart{VehicleID: vehicleID, Time: acStart, BatteryLevel: 50, RangeKm: 200})
	if err != nil {
		t.Fatalf("open ac session: %v", err)
	}
	if err := s.AppendChargingSample(ChargingSample{
		ChargingSessionID: acID, VehicleID: vehicleID, Time: acStart.Add(time.Minute),
		BatteryLevel: 60, FastChargerPresent: false, RangeKm: 240,
	}); err != nil {
		t.Fatalf("append ac sample: %v", err)
	}
	if err := s.CloseChargingSession(ChargeEnd{ChargingSessionID: acID, Time: acStart.Add(4 * time.Hour), BatteryLevel: 80, RangeKm: 320, EnergyAddedKwh: 24}); err != nil {
		t.Fatalf("close ac session: %v", err)
	}

	charges, err := s.ListCharges(vehicleID, 0)
	if err != nil {
		t.Fatalf("list charges: %v", err)
	}
	if len(charges) != 2 {
		t.Fatalf("expected 2 charges, got %d", len(charges))
	}
	// Newest first: AC session, then DC session.
	if charges[0].ChargeType() != "AC" {
		t.Fatalf("expected first (newest) charge to be AC, got %s", charges[0].ChargeType())
	}
	if charges[1].ChargeType() != "DC" {
		t.Fatalf("expected second (oldest) charge to be DC, got %s", charges[1].ChargeType())
	}
	if got := charges[1].KwhPerRatedKm(); got <= 0 {
		t.Fatalf("expected a positive kWh/rated-km for the DC session, got %.3f", got)
	}
}

func TestDriveSummaryEfficiencyRatio(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN8", TeslaID: "8", DisplayName: "Car"})

	start := time.Now().UTC()
	driveID, err := s.OpenDrive(DriveStart{VehicleID: vehicleID, Time: start, OdometerKm: 100, RangeKm: 300})
	if err != nil {
		t.Fatalf("open drive: %v", err)
	}
	for i, odo := range []float64{101, 105, 110} {
		if err := s.AppendPosition(PositionSample{DriveID: driveID, VehicleID: vehicleID, Time: start.Add(time.Duration(i+1) * 3 * time.Minute), OdometerKm: odo, ShiftState: "D"}); err != nil {
			t.Fatalf("append position %d: %v", i, err)
		}
	}
	if err := s.CloseDrive(DriveEnd{DriveID: driveID, Time: start.Add(10 * time.Minute), OdometerKm: 110, RangeKm: 290}); err != nil {
		t.Fatalf("close drive: %v", err)
	}

	drives, err := s.ListDrives(vehicleID, 0)
	if err != nil {
		t.Fatalf("list drives: %v", err)
	}
	if len(drives) != 1 {
		t.Fatalf("expected 1 drive, got %d", len(drives))
	}
	d := drives[0]
	if d.RangeLostKm() != 10 {
		t.Fatalf("expected 10 rated-range km lost, got %.2f", d.RangeLostKm())
	}
	// Drove 10km, lost 10 rated-range km: ratio should be ~1.0.
	if diff := d.EfficiencyRatio() - 1.0; diff > 0.01 || diff < -0.01 {
		t.Fatalf("expected efficiency ratio ~1.0, got %.3f", d.EfficiencyRatio())
	}
}

func TestUpdateVehicleFirmwareIgnoresEmptyVersion(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN10", TeslaID: "10", DisplayName: "Car"})

	if err := s.UpdateVehicleFirmware(vehicleID, "2026.20.1"); err != nil {
		t.Fatalf("update firmware: %v", err)
	}
	// An empty version (some vehicle_data responses omit car_version)
	// must not clobber the last known value.
	if err := s.UpdateVehicleFirmware(vehicleID, ""); err != nil {
		t.Fatalf("update firmware with empty version: %v", err)
	}

	var version string
	if err := s.db.QueryRow(`SELECT firmware_version FROM vehicles WHERE id = ?`, vehicleID).Scan(&version); err != nil {
		t.Fatalf("query firmware_version: %v", err)
	}
	if version != "2026.20.1" {
		t.Fatalf("expected firmware_version to survive an empty update, got %q", version)
	}
}

func TestLifetimeAggregatesDrivesAndCharges(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN11", TeslaID: "11", DisplayName: "Car"})

	now := time.Now().UTC()
	driveID, err := s.OpenDrive(DriveStart{VehicleID: vehicleID, Time: now, OdometerKm: 1000})
	if err != nil {
		t.Fatalf("open drive: %v", err)
	}
	for i, odo := range []float64{1005, 1012, 1020} {
		if err := s.AppendPosition(PositionSample{DriveID: driveID, VehicleID: vehicleID, Time: now.Add(time.Duration(i+1) * 3 * time.Minute), OdometerKm: odo, ShiftState: "D"}); err != nil {
			t.Fatalf("append position %d: %v", i, err)
		}
	}
	if err := s.CloseDrive(DriveEnd{DriveID: driveID, Time: now.Add(10 * time.Minute), OdometerKm: 1020}); err != nil {
		t.Fatalf("close drive: %v", err)
	}

	chargeID, err := s.OpenChargingSession(ChargeStart{VehicleID: vehicleID, Time: now, BatteryLevel: 20})
	if err != nil {
		t.Fatalf("open charge: %v", err)
	}
	if err := s.CloseChargingSession(ChargeEnd{ChargingSessionID: chargeID, Time: now.Add(20 * time.Minute), BatteryLevel: 80, EnergyAddedKwh: 30}); err != nil {
		t.Fatalf("close charge: %v", err)
	}

	lt, err := s.Lifetime(vehicleID)
	if err != nil {
		t.Fatalf("lifetime: %v", err)
	}
	if lt.OdometerKm != 1020 {
		t.Fatalf("expected lifetime odometer 1020, got %.1f", lt.OdometerKm)
	}
	if lt.TotalDrives != 1 || lt.TotalKm != 20 {
		t.Fatalf("expected 1 drive / 20km, got %d drives / %.1fkm", lt.TotalDrives, lt.TotalKm)
	}
	if lt.TotalCharges != 1 || lt.TotalKwh != 30 {
		t.Fatalf("expected 1 charge / 30kWh, got %d charges / %.1fkWh", lt.TotalCharges, lt.TotalKwh)
	}
}

func TestSleepStats24hSplitsAsleepVsAwakeTime(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN12", TeslaID: "12", DisplayName: "Car"})

	now := time.Now().UTC()
	windowStart := now.Add(-24 * time.Hour)
	// Exactly at window start: online for 6h, then asleep for the
	// remaining 18h up to now - covers the entire 24h window with no
	// gap, so the split is unambiguous.
	asleepStart := windowStart.Add(6 * time.Hour)
	if _, err := s.OpenState(vehicleID, "online", windowStart); err != nil {
		t.Fatalf("open online state: %v", err)
	}
	if _, err := s.OpenState(vehicleID, "asleep", asleepStart); err != nil {
		t.Fatalf("open asleep state: %v", err)
	}

	stats, err := s.SleepStats24h(vehicleID, now)
	if err != nil {
		t.Fatalf("sleep stats: %v", err)
	}
	if diff := stats.WindowHours - 24; diff > 0.01 || diff < -0.01 {
		t.Fatalf("expected a 24h window, got %.2f", stats.WindowHours)
	}
	if diff := stats.AsleepHours - 18; diff > 0.05 || diff < -0.05 {
		t.Fatalf("expected ~18h asleep, got %.2f", stats.AsleepHours)
	}
	if diff := stats.AwakeHours() - 6; diff > 0.05 || diff < -0.05 {
		t.Fatalf("expected ~6h awake, got %.2f", stats.AwakeHours())
	}
	if diff := stats.AsleepPct() - 75; diff > 0.5 || diff < -0.5 {
		t.Fatalf("expected ~75%% asleep, got %.1f", stats.AsleepPct())
	}
}

func TestLatestBatteryReadingPrefersOpenSessionOverIdleSample(t *testing.T) {
	s := openTestStore(t)
	vehicleID, _ := s.UpsertVehicle(VehicleMeta{VIN: "VIN9", TeslaID: "9", DisplayName: "Car"})

	if ok, _, _, _, _, err := s.LatestBatteryReading(vehicleID); err != nil || ok {
		t.Fatalf("expected no reading yet, got ok=%v err=%v", ok, err)
	}

	now := time.Now().UTC()
	if err := s.InsertBatterySample(vehicleID, now, 55, 220, 230, "poll"); err != nil {
		t.Fatalf("insert battery sample: %v", err)
	}
	if ok, level, rangeKm, _, _, err := s.LatestBatteryReading(vehicleID); err != nil || !ok || level != 55 || rangeKm != 220 {
		t.Fatalf("expected idle reading level=55 range=220, got ok=%v level=%d range=%.1f err=%v", ok, level, rangeKm, err)
	}

	driveID, err := s.OpenDrive(DriveStart{VehicleID: vehicleID, Time: now.Add(time.Minute), OdometerKm: 10, RangeKm: 220})
	if err != nil {
		t.Fatalf("open drive: %v", err)
	}
	if err := s.AppendPosition(PositionSample{
		DriveID: driveID, VehicleID: vehicleID, Time: now.Add(2 * time.Minute),
		BatteryLevel: 52, RangeKm: 210,
	}); err != nil {
		t.Fatalf("append position: %v", err)
	}

	ok, level, rangeKm, _, _, err := s.LatestBatteryReading(vehicleID)
	if err != nil || !ok || level != 52 || rangeKm != 210 {
		t.Fatalf("expected in-drive reading level=52 range=210, got ok=%v level=%d range=%.1f err=%v", ok, level, rangeKm, err)
	}
}
