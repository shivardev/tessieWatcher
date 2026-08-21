// Package storage is the SQLite persistence layer for teslalog.
//
// It uses ncruces/go-sqlite3, a pure-Go SQLite build (SQLite compiled
// to WebAssembly, embedded in the module, and run via the wazero
// runtime) instead of a cgo binding. That means CGO_ENABLED=0 and no C
// cross-compiler or target-side glibc version to worry about at all —
// cross-compiling for the Raspberry Pi Zero 2 W (linux/arm64) is a
// plain `GOOS=linux GOARCH=arm64 go build`, and the resulting binary
// has no dynamic library dependencies beyond the Linux syscall ABI.
package storage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
)

const timeLayout = time.RFC3339Nano

// Store wraps a SQLite connection configured for a single-writer,
// embedded-logger workload (WAL journal, NORMAL synchronous, foreign
// keys enforced, single connection to avoid SQLITE_BUSY under WAL with
// this driver).
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path,
// applies pragmas, and ensures the schema exists.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	// A single logical writer: keep exactly one connection so WAL mode
	// never has to arbitrate between concurrent *database/sql* pooled
	// connections in this process (SQLite itself still allows readers
	// during a writer transaction under WAL).
	db.SetMaxOpenConns(1)

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA busy_timeout = 5000;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply pragma %q: %w", p, err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	if err := applyColumnMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply column migrations: %w", err)
	}

	return &Store{db: db}, nil
}

// columnMigrations adds columns introduced after a table's initial
// CREATE TABLE that already shipped in a release - `schema`'s own
// CREATE TABLE IF NOT EXISTS only ever applies to a brand-new database;
// it does nothing for a table that already exists on disk (e.g. an
// already-deployed teslalog's tesla.db), so those need an explicit ADD
// COLUMN here instead. Append-only: never remove or reorder entries,
// since this list runs unconditionally on every startup against
// databases at any prior version.
var columnMigrations = []string{
	`ALTER TABLE drives ADD COLUMN start_location TEXT`,
	`ALTER TABLE drives ADD COLUMN end_location TEXT`,
	`ALTER TABLE charging_sessions ADD COLUMN location TEXT`,
	`ALTER TABLE positions ADD COLUMN sentry_mode INTEGER`,
	`ALTER TABLE positions ADD COLUMN is_user_present INTEGER`,
	`ALTER TABLE positions ADD COLUMN valet_mode INTEGER`,
	`ALTER TABLE positions ADD COLUMN climate_keeper_mode TEXT`,
	`ALTER TABLE charging_samples ADD COLUMN charge_limit_soc INTEGER`,
}

func applyColumnMigrations(db *sql.DB) error {
	for _, stmt := range columnMigrations {
		if _, err := db.Exec(stmt); err != nil {
			// SQLite's error for a column that's already there (i.e. this
			// migration already ran on a prior startup, or the table was
			// just freshly CREATEd with the column already in it) - the
			// one error this loop must swallow to stay idempotent.
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB exposes the underlying *sql.DB, e.g. for the backup package's
// SQLite online backup, or for ad-hoc export queries.
func (s *Store) DB() *sql.DB {
	return s.db
}

func fmtTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- vehicles ----

// VehicleMeta is the API-derived (not user-configured) vehicle identity
// fields upserted whenever we see them.
type VehicleMeta struct {
	VIN         string
	TeslaID     string
	DisplayName string
	// Model, TrimBadging and MarketingName should already be the
	// normalized/derived values from internal/tesla.IdentifyVehicle
	// (matching TeslaMate's own cars table), not raw car_type/
	// trim_badging API strings.
	Model         string
	TrimBadging   string
	MarketingName string
	ExteriorColor string
	WheelType     string
	SpoilerType   string
}

// UpsertVehicle ensures a vehicle row exists for vin and returns its id.
func (s *Store) UpsertVehicle(v VehicleMeta) (int64, error) {
	_, err := s.db.Exec(`
		INSERT INTO vehicles (vin, tesla_id, display_name, model, trim_badging, marketing_name, exterior_color, wheel_type, spoiler_type)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(vin) DO UPDATE SET
			tesla_id = excluded.tesla_id,
			display_name = excluded.display_name,
			model = excluded.model,
			trim_badging = excluded.trim_badging,
			marketing_name = excluded.marketing_name,
			exterior_color = excluded.exterior_color,
			wheel_type = excluded.wheel_type,
			spoiler_type = excluded.spoiler_type
	`, v.VIN, v.TeslaID, v.DisplayName, v.Model, v.TrimBadging, v.MarketingName, v.ExteriorColor, v.WheelType, v.SpoilerType)
	if err != nil {
		return 0, fmt.Errorf("upsert vehicle: %w", err)
	}

	var id int64
	if err := s.db.QueryRow(`SELECT id FROM vehicles WHERE vin = ?`, v.VIN).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup vehicle id: %w", err)
	}
	return id, nil
}

// SetVehicleEfficiency stores a user-configured Wh/km efficiency
// estimate (config.toml's [vehicle].efficiency_wh_km) — not reported by
// the API, used the same way TeslaMate uses it: to project range from
// battery percentage.
func (s *Store) SetVehicleEfficiency(vehicleID int64, whPerKm float64) error {
	_, err := s.db.Exec(`UPDATE vehicles SET efficiency_wh_km = ? WHERE id = ?`, whPerKm, vehicleID)
	return err
}

// ---- state machine history ----

// OpenState closes any currently-open state row for vehicleID and opens
// a new one. Returns the new state row id.
func (s *Store) OpenState(vehicleID int64, state string, at time.Time) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		UPDATE states SET ended_at = ?
		WHERE vehicle_id = ? AND ended_at IS NULL
	`, fmtTime(at), vehicleID); err != nil {
		return 0, fmt.Errorf("close previous state: %w", err)
	}

	res, err := tx.Exec(`
		INSERT INTO states (vehicle_id, state, started_at) VALUES (?, ?, ?)
	`, vehicleID, state, fmtTime(at))
	if err != nil {
		return 0, fmt.Errorf("insert state: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// CurrentState returns the most recent (open or last) state string for
// a vehicle, or "" if none recorded yet.
func (s *Store) CurrentState(vehicleID int64) (string, error) {
	var state string
	err := s.db.QueryRow(`
		SELECT state FROM states WHERE vehicle_id = ?
		ORDER BY started_at DESC LIMIT 1
	`, vehicleID).Scan(&state)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return state, nil
}

// ---- drives ----

type DriveStart struct {
	VehicleID    int64
	Time         time.Time
	OdometerKm   float64
	BatteryLevel int
	RangeKm      float64
	IdealRangeKm float64
	Lat, Lng     float64
	// StartLocation is a resolved place name (config.toml [[geofence]]
	// match, or reverse-geocoded if [geocoding].enabled) - empty string
	// stores as NULL, meaning neither resolved anything.
	StartLocation string
}

// OpenDrive inserts a new open drive row and returns its id.
func (s *Store) OpenDrive(d DriveStart) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO drives (
			vehicle_id, start_time, start_odometer_km, start_battery_level,
			start_range_km, start_ideal_range_km, start_lat, start_lng, start_location, status
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'open')
	`, d.VehicleID, fmtTime(d.Time), d.OdometerKm, d.BatteryLevel, d.RangeKm, d.IdealRangeKm, d.Lat, d.Lng, nullIfEmpty(d.StartLocation))
	if err != nil {
		return 0, fmt.Errorf("open drive: %w", err)
	}
	return res.LastInsertId()
}

// nullIfEmpty turns an empty string into a real SQL NULL rather than
// storing a misleading empty string for "no location resolved".
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// PositionSample is one GPS/telemetry sample. Field set mirrors
// TeslaMate's positions table (lib/teslamate/log/position.ex).
//
// ElevationM, OutsideTempC and InsideTempC are *float64 (nil = unknown),
// not plain float64, because they are NOT always available: REST-derived
// samples (positionFromSnapshot in internal/runner) never carry elevation
// (the Owner API doesn't report it), and streaming-derived samples
// (drainStream) never carry climate data (Tesla's legacy stream protocol
// doesn't include it). Storing a bare 0.0 zero-value for "unknown" instead
// of a real SQL NULL would silently corrupt CloseDrive's
// AVG(outside_temp_c)/AVG(inside_temp_c) and elevationChange's ascent/
// descent calculation (which already, correctly, filters on
// `elevation_m IS NOT NULL`) by mixing real readings with phantom zeros.
type PositionSample struct {
	DriveID    int64
	VehicleID  int64
	Time       time.Time
	Lat, Lng   float64
	SpeedKmh   float64
	Heading    float64
	ElevationM *float64
	PowerKw    float64
	OdometerKm float64

	BatteryLevel       int
	UsableBatteryLevel int
	RangeKm            float64
	IdealRangeKm       float64
	EstRangeKm         float64
	BatteryHeaterOn    bool

	OutsideTempC          *float64
	InsideTempC           *float64
	FanStatus             int
	DriverTempSettingC    float64
	PassengerTempSettingC float64
	IsClimateOn           bool
	IsRearDefrosterOn     bool
	IsFrontDefrosterOn    bool

	TpmsPressureFL, TpmsPressureFR, TpmsPressureRL, TpmsPressureRR float64

	ShiftState string

	// Not tracked by TeslaMate at all - see schema.go's positions table
	// comment for why they're here anyway.
	SentryMode        bool
	IsUserPresent     bool
	ValetMode         bool
	ClimateKeeperMode string
}

// AppendPosition records one GPS/telemetry sample for an open drive.
func (s *Store) AppendPosition(p PositionSample) error {
	_, err := s.db.Exec(`
		INSERT INTO positions (
			drive_id, vehicle_id, timestamp, latitude, longitude, speed_kmh,
			heading, elevation_m, power_kw, odometer_km,
			battery_level, usable_battery_level, range_km, ideal_range_km, est_range_km, battery_heater_on,
			outside_temp_c, inside_temp_c, fan_status, driver_temp_setting_c, passenger_temp_setting_c,
			is_climate_on, is_rear_defroster_on, is_front_defroster_on,
			tpms_pressure_fl, tpms_pressure_fr, tpms_pressure_rl, tpms_pressure_rr,
			shift_state, sentry_mode, is_user_present, valet_mode, climate_keeper_mode
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.DriveID, p.VehicleID, fmtTime(p.Time), p.Lat, p.Lng, p.SpeedKmh,
		p.Heading, p.ElevationM, p.PowerKw, p.OdometerKm,
		p.BatteryLevel, p.UsableBatteryLevel, p.RangeKm, p.IdealRangeKm, p.EstRangeKm, boolToInt(p.BatteryHeaterOn),
		p.OutsideTempC, p.InsideTempC, p.FanStatus, p.DriverTempSettingC, p.PassengerTempSettingC,
		boolToInt(p.IsClimateOn), boolToInt(p.IsRearDefrosterOn), boolToInt(p.IsFrontDefrosterOn),
		p.TpmsPressureFL, p.TpmsPressureFR, p.TpmsPressureRL, p.TpmsPressureRR,
		p.ShiftState, boolToInt(p.SentryMode), boolToInt(p.IsUserPresent), boolToInt(p.ValetMode), nullIfEmpty(p.ClimateKeeperMode))
	if err != nil {
		return fmt.Errorf("append position: %w", err)
	}
	return nil
}

type DriveEnd struct {
	DriveID      int64
	Time         time.Time
	OdometerKm   float64
	BatteryLevel int
	RangeKm      float64
	IdealRangeKm float64
	Lat, Lng     float64
	// EndLocation - see DriveStart.StartLocation's doc comment.
	EndLocation string
}

// CloseDrive finalizes a drive: sets end fields, computed distance,
// duration, and aggregate stats (max speed, max/min power, avg outside/
// inside temp, ascent/descent), then marks status 'closed'. Aggregates
// are derived from the recorded positions plus start/end odometer so
// the numbers are correct even if some samples were missed.
func (s *Store) CloseDrive(e DriveEnd) error {
	var startTimeStr string
	var startOdometer float64
	if err := s.db.QueryRow(`
		SELECT start_time, start_odometer_km FROM drives WHERE id = ?
	`, e.DriveID).Scan(&startTimeStr, &startOdometer); err != nil {
		return fmt.Errorf("load drive %d: %w", e.DriveID, err)
	}
	startTime, err := time.Parse(timeLayout, startTimeStr)
	if err != nil {
		return fmt.Errorf("parse start_time: %w", err)
	}

	distance := e.OdometerKm - startOdometer
	if distance < 0 {
		distance = 0
	}
	duration := e.Time.Sub(startTime).Minutes()

	var maxSpeed, maxPower, minPower, avgOutside, avgInside sql.NullFloat64
	_ = s.db.QueryRow(`
		SELECT MAX(speed_kmh), MAX(power_kw), MIN(power_kw), AVG(outside_temp_c), AVG(inside_temp_c)
		FROM positions WHERE drive_id = ?
	`, e.DriveID).Scan(&maxSpeed, &maxPower, &minPower, &avgOutside, &avgInside)

	ascent, descent, err := s.elevationChange(e.DriveID)
	if err != nil {
		return fmt.Errorf("compute elevation change: %w", err)
	}

	_, err = s.db.Exec(`
		UPDATE drives SET
			end_time = ?, end_odometer_km = ?, end_battery_level = ?,
			end_range_km = ?, end_ideal_range_km = ?, end_lat = ?, end_lng = ?, end_location = ?,
			distance_km = ?, duration_min = ?, max_speed_kmh = ?,
			max_power_kw = ?, min_power_kw = ?, outside_temp_avg_c = ?, inside_temp_avg_c = ?,
			ascent_m = ?, descent_m = ?, status = 'closed'
		WHERE id = ?
	`, fmtTime(e.Time), e.OdometerKm, e.BatteryLevel, e.RangeKm, e.IdealRangeKm, e.Lat, e.Lng, nullIfEmpty(e.EndLocation),
		distance, duration, maxSpeed.Float64,
		maxPower.Float64, minPower.Float64, avgOutside.Float64, avgInside.Float64,
		ascent, descent, e.DriveID)
	if err != nil {
		return fmt.Errorf("close drive %d: %w", e.DriveID, err)
	}
	return nil
}

// elevationChange sums positive (ascent) and negative (descent, as a
// positive magnitude) deltas between consecutive elevation_m readings
// for a drive, skipping NULLs (elevation only comes from the streaming
// client; if streaming was never connected during the drive, both
// return 0, not an error).
func (s *Store) elevationChange(driveID int64) (ascent, descent float64, err error) {
	rows, err := s.db.Query(`
		SELECT elevation_m FROM positions
		WHERE drive_id = ? AND elevation_m IS NOT NULL
		ORDER BY timestamp
	`, driveID)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	var prev float64
	havePrev := false
	for rows.Next() {
		var elev float64
		if err := rows.Scan(&elev); err != nil {
			return 0, 0, err
		}
		if havePrev {
			if d := elev - prev; d > 0 {
				ascent += d
			} else {
				descent += -d
			}
		}
		prev = elev
		havePrev = true
	}
	return ascent, descent, rows.Err()
}

// OpenDriveID returns the id of the currently open drive for a vehicle,
// or 0 if there is none.
func (s *Store) OpenDriveID(vehicleID int64) (int64, error) {
	var id int64
	err := s.db.QueryRow(`
		SELECT id FROM drives WHERE vehicle_id = ? AND status = 'open'
		ORDER BY start_time DESC LIMIT 1
	`, vehicleID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// ---- charging sessions ----

type ChargeStart struct {
	VehicleID    int64
	Time         time.Time
	BatteryLevel int
	RangeKm      float64
	IdealRangeKm float64
	Lat, Lng     float64
	// Location - see DriveStart.StartLocation's doc comment. A charging
	// session only gets one (unlike a drive's separate start/end), since
	// it happens in one place.
	Location string
}

func (s *Store) OpenChargingSession(c ChargeStart) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO charging_sessions (
			vehicle_id, start_time, start_battery_level, start_range_km, start_ideal_range_km,
			latitude, longitude, location, status
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'open')
	`, c.VehicleID, fmtTime(c.Time), c.BatteryLevel, c.RangeKm, c.IdealRangeKm, c.Lat, c.Lng, nullIfEmpty(c.Location))
	if err != nil {
		return 0, fmt.Errorf("open charging session: %w", err)
	}
	return res.LastInsertId()
}

// ChargingSample is one charging telemetry sample. Field set mirrors
// TeslaMate's charges table (lib/teslamate/log/charge.ex).
type ChargingSample struct {
	ChargingSessionID int64
	VehicleID         int64
	Time              time.Time

	BatteryLevel       int
	UsableBatteryLevel int

	ChargerPowerKw      float64
	ChargerVoltage      float64
	ChargerCurrent      float64
	ChargerPilotCurrent int
	ChargerPhases       int
	ConnChargeCable     string
	FastChargerPresent  bool
	FastChargerBrand    string
	FastChargerType     string

	EnergyAddedKwh       float64
	RangeKm              float64
	IdealRangeKm         float64
	BatteryHeaterOn      bool
	NotEnoughPowerToHeat bool
	OutsideTempC         float64
	// ChargeLimitSoc - not tracked by TeslaMate at all, see schema.go's
	// charging_samples table comment for why it's worth having anyway.
	ChargeLimitSoc int
}

func (s *Store) AppendChargingSample(c ChargingSample) error {
	_, err := s.db.Exec(`
		INSERT INTO charging_samples (
			charging_session_id, vehicle_id, timestamp,
			battery_level, usable_battery_level,
			charger_power_kw, charger_voltage, charger_actual_current, charger_pilot_current,
			charger_phases, conn_charge_cable, fast_charger_present, fast_charger_brand, fast_charger_type,
			charge_energy_added_kwh, range_km, ideal_range_km,
			battery_heater_on, not_enough_power_to_heat, outside_temp_c, charge_limit_soc
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, c.ChargingSessionID, c.VehicleID, fmtTime(c.Time),
		c.BatteryLevel, c.UsableBatteryLevel,
		c.ChargerPowerKw, c.ChargerVoltage, c.ChargerCurrent, c.ChargerPilotCurrent,
		c.ChargerPhases, c.ConnChargeCable, boolToInt(c.FastChargerPresent), c.FastChargerBrand, c.FastChargerType,
		c.EnergyAddedKwh, c.RangeKm, c.IdealRangeKm,
		boolToInt(c.BatteryHeaterOn), boolToInt(c.NotEnoughPowerToHeat), c.OutsideTempC, c.ChargeLimitSoc)
	return err
}

type ChargeEnd struct {
	ChargingSessionID int64
	Time              time.Time
	BatteryLevel      int
	RangeKm           float64
	IdealRangeKm      float64
	EnergyAddedKwh    float64

	// ChargingEfficiency (0-1), if > 0, is used to estimate
	// charge_energy_used_kwh = EnergyAddedKwh / ChargingEfficiency —
	// the API only reports energy *added* to the battery, not energy
	// drawn from the wall; TeslaMate models the difference the same
	// way, via a configurable loss factor. Leave 0 to skip.
	ChargingEfficiency float64

	// PricePerKwh, if > 0, is used to compute cost = EnergyAddedKwh *
	// PricePerKwh. Leave 0 to skip (matches TeslaMate's behavior
	// without a configured geofence price).
	PricePerKwh float64
}

func (s *Store) CloseChargingSession(e ChargeEnd) error {
	var maxPower, avgOutside sql.NullFloat64
	_ = s.db.QueryRow(`
		SELECT MAX(charger_power_kw), AVG(outside_temp_c) FROM charging_samples WHERE charging_session_id = ?
	`, e.ChargingSessionID).Scan(&maxPower, &avgOutside)

	var energyUsed sql.NullFloat64
	if e.ChargingEfficiency > 0 {
		energyUsed = sql.NullFloat64{Float64: e.EnergyAddedKwh / e.ChargingEfficiency, Valid: true}
	}
	var cost sql.NullFloat64
	if e.PricePerKwh > 0 {
		cost = sql.NullFloat64{Float64: e.EnergyAddedKwh * e.PricePerKwh, Valid: true}
	}

	_, err := s.db.Exec(`
		UPDATE charging_sessions SET
			end_time = ?, end_battery_level = ?, end_range_km = ?, end_ideal_range_km = ?,
			charge_energy_added_kwh = ?, charge_energy_used_kwh = ?, max_charger_power_kw = ?,
			outside_temp_avg_c = ?, cost = ?, status = 'closed'
		WHERE id = ?
	`, fmtTime(e.Time), e.BatteryLevel, e.RangeKm, e.IdealRangeKm,
		e.EnergyAddedKwh, energyUsed, maxPower.Float64,
		avgOutside.Float64, cost, e.ChargingSessionID)
	return err
}

// OpenChargingSessionID returns the id of the currently open charging
// session for a vehicle, or 0 if there is none.
func (s *Store) OpenChargingSessionID(vehicleID int64) (int64, error) {
	var id int64
	err := s.db.QueryRow(`
		SELECT id FROM charging_sessions WHERE vehicle_id = ? AND status = 'open'
		ORDER BY start_time DESC LIMIT 1
	`, vehicleID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// ---- battery samples ----

func (s *Store) InsertBatterySample(vehicleID int64, at time.Time, level int, rangeKm, idealRangeKm float64, source string) error {
	_, err := s.db.Exec(`
		INSERT INTO battery_samples (vehicle_id, timestamp, battery_level, battery_range_km, ideal_battery_range_km, source)
		VALUES (?, ?, ?, ?, ?, ?)
	`, vehicleID, fmtTime(at), level, rangeKm, idealRangeKm, source)
	return err
}

// ---- software updates ----

func (s *Store) UpsertSoftwareUpdateStart(vehicleID int64, version string, at time.Time) error {
	var existing int64
	err := s.db.QueryRow(`
		SELECT id FROM software_updates WHERE vehicle_id = ? AND version = ? AND end_time IS NULL
	`, vehicleID, version).Scan(&existing)
	if err == nil {
		return nil // already tracking this update
	}
	_, err = s.db.Exec(`
		INSERT INTO software_updates (vehicle_id, version, status, start_time) VALUES (?, ?, 'installing', ?)
	`, vehicleID, version, fmtTime(at))
	return err
}

func (s *Store) CompleteSoftwareUpdate(vehicleID int64, version string, at time.Time) error {
	_, err := s.db.Exec(`
		UPDATE software_updates SET status = 'installed', end_time = ?
		WHERE vehicle_id = ? AND version = ? AND end_time IS NULL
	`, fmtTime(at), vehicleID, version)
	return err
}

// ---- geocode cache ----
//
// *Store satisfies internal/geocode.Cache directly via these two
// methods, so the geocode package stays decoupled from storage (no
// import of internal/storage from internal/geocode) while still
// getting real persistence across restarts.

// Lookup returns a previously-cached place name for the rounded
// coordinate pair (latKey, lngKey; see internal/geocode.roundCoord),
// or ok=false if nothing's cached for it yet.
func (s *Store) Lookup(latKey, lngKey float64) (name string, ok bool, err error) {
	err = s.db.QueryRow(`
		SELECT name FROM geocode_cache WHERE lat_key = ? AND lng_key = ?
	`, latKey, lngKey).Scan(&name)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}

// Save persists a resolved place name for the rounded coordinate pair.
func (s *Store) Save(latKey, lngKey float64, name string) error {
	_, err := s.db.Exec(`
		INSERT INTO geocode_cache (lat_key, lng_key, name) VALUES (?, ?, ?)
		ON CONFLICT(lat_key, lng_key) DO UPDATE SET name = excluded.name
	`, latKey, lngKey, name)
	return err
}

// ---- export/reporting helpers ----

type DriveSummary struct {
	ID           int64
	StartTime    string
	EndTime      string
	DistanceKm   float64
	DurationMin  float64
	StartBattery int
	EndBattery   int
	// StartLocation/EndLocation are resolved place names (see
	// DriveStart.StartLocation's doc comment) - empty if neither a
	// configured geofence nor reverse-geocoding resolved anything.
	StartLocation string
	EndLocation   string
}

// ListDrives returns closed drives for a vehicle, optionally filtered
// by year (pass 0 for all years), newest first.
func (s *Store) ListDrives(vehicleID int64, year int) ([]DriveSummary, error) {
	q := `
		SELECT id, start_time, end_time, distance_km, duration_min, start_battery_level, end_battery_level,
			start_location, end_location
		FROM drives
		WHERE vehicle_id = ? AND status = 'closed'
	`
	args := []any{vehicleID}
	if year != 0 {
		q += ` AND strftime('%Y', start_time) = ?`
		args = append(args, fmt.Sprintf("%04d", year))
	}
	q += ` ORDER BY start_time DESC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DriveSummary
	for rows.Next() {
		var d DriveSummary
		var endTime sql.NullString
		var distance, duration sql.NullFloat64
		var startBattery, endBattery sql.NullInt64
		var startLoc, endLoc sql.NullString
		if err := rows.Scan(&d.ID, &d.StartTime, &endTime, &distance, &duration, &startBattery, &endBattery, &startLoc, &endLoc); err != nil {
			return nil, err
		}
		d.EndTime = endTime.String
		d.DistanceKm = distance.Float64
		d.DurationMin = duration.Float64
		d.StartBattery = int(startBattery.Int64)
		d.EndBattery = int(endBattery.Int64)
		d.StartLocation = startLoc.String
		d.EndLocation = endLoc.String
		out = append(out, d)
	}
	return out, rows.Err()
}

type ChargeSummary struct {
	ID             int64
	StartTime      string
	EndTime        string
	StartBattery   int
	EndBattery     int
	EnergyAddedKwh float64
	// Location - see ChargeStart.Location's doc comment.
	Location string
}

func (s *Store) ListCharges(vehicleID int64, year int) ([]ChargeSummary, error) {
	q := `
		SELECT id, start_time, end_time, start_battery_level, end_battery_level, charge_energy_added_kwh, location
		FROM charging_sessions
		WHERE vehicle_id = ? AND status = 'closed'
	`
	args := []any{vehicleID}
	if year != 0 {
		q += ` AND strftime('%Y', start_time) = ?`
		args = append(args, fmt.Sprintf("%04d", year))
	}
	q += ` ORDER BY start_time DESC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChargeSummary
	for rows.Next() {
		var c ChargeSummary
		var endTime sql.NullString
		var startBattery, endBattery sql.NullInt64
		var energy sql.NullFloat64
		var loc sql.NullString
		if err := rows.Scan(&c.ID, &c.StartTime, &endTime, &startBattery, &endBattery, &energy, &loc); err != nil {
			return nil, err
		}
		c.Location = loc.String
		c.EndTime = endTime.String
		c.StartBattery = int(startBattery.Int64)
		c.EndBattery = int(endBattery.Int64)
		c.EnergyAddedKwh = energy.Float64
		out = append(out, c)
	}
	return out, rows.Err()
}
