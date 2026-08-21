package storage

import (
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

	if err := s.AppendPosition(PositionSample{
		DriveID: driveID, VehicleID: vehicleID, Time: time.Now().UTC(),
		SentryMode: true, IsUserPresent: true, ValetMode: false, ClimateKeeperMode: "dog",
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
