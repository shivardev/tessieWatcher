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
	`ALTER TABLE charging_sessions ADD COLUMN is_dc_fast_charge INTEGER`,
	`ALTER TABLE vehicles ADD COLUMN firmware_version TEXT`,
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

// boolPtrToInt is boolToInt for a nullable bool - nil stays nil (SQL
// NULL) rather than silently becoming boolToInt(false) (0). Needed
// because SQLite has no native bool type (every bool column is stored
// as 0/1), so the nil-preserving behavior *float64/*int params get for
// free from database/sql needs the same thing spelled out by hand here.
func boolPtrToInt(b *bool) any {
	if b == nil {
		return nil
	}
	return boolToInt(*b)
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

// UpdateVehicleFirmware records the car's currently-installed software
// version (vehicle_state.car_version - see vehicle.Snapshot.Firmware's
// doc comment). Call opportunistically on any poll that reports one;
// a no-op if version is empty (some vehicle_data responses omit it).
func (s *Store) UpdateVehicleFirmware(vehicleID int64, version string) error {
	if version == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE vehicles SET firmware_version = ? WHERE id = ?`, version, vehicleID)
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
// PositionSample records one GPS/telemetry sample for an open drive.
// Fields are *pointer* types wherever the value can genuinely be
// unknown for a given sample - specifically, everywhere the streaming
// client's tesla.StreamSample (see stream.go) doesn't carry that
// field at all, since only REST vehicle_data polls report it
// (fan/climate/defroster state, TPMS, usable battery %, ideal/
// estimated range, sentry/valet/user-present). A nil pointer here
// means "this sample's source didn't report it," stored as SQL NULL -
// NOT "known to be 0/off". Getting this wrong is a real, previously-
// shipped bug: with these as plain (non-pointer) fields, every
// streaming-sourced sample - the large majority of samples during any
// drive, since streaming reports far more frequently than the REST
// poll interval - silently wrote Go's zero value (fan_status=0,
// climate=off, all four tire pressures=0, etc.) instead of leaving the
// column NULL, which is indistinguishable from "really is off/zero"
// to anything reading the data back out.
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

	// BatteryLevel and RangeKm (rated range) are reported by both REST
	// and streaming, so these stay plain, always-known fields.
	BatteryLevel int
	RangeKm      float64

	UsableBatteryLevel *int
	IdealRangeKm       *float64
	EstRangeKm         *float64
	BatteryHeaterOn    *bool

	OutsideTempC          *float64
	InsideTempC           *float64
	FanStatus             *int
	DriverTempSettingC    *float64
	PassengerTempSettingC *float64
	IsClimateOn           *bool
	IsRearDefrosterOn     *bool
	IsFrontDefrosterOn    *bool

	TpmsPressureFL, TpmsPressureFR, TpmsPressureRL, TpmsPressureRR *float64

	// ShiftState is reported by both sources ("" is a legitimate "no
	// gear info yet" value either way, not a source limitation), so it
	// stays a plain string.
	ShiftState string

	// Not tracked by TeslaMate at all - see schema.go's positions table
	// comment for why they're here anyway. Also REST-only, like the
	// climate/TPMS fields above.
	SentryMode        *bool
	IsUserPresent     *bool
	ValetMode         *bool
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
		p.BatteryLevel, p.UsableBatteryLevel, p.RangeKm, p.IdealRangeKm, p.EstRangeKm, boolPtrToInt(p.BatteryHeaterOn),
		p.OutsideTempC, p.InsideTempC, p.FanStatus, p.DriverTempSettingC, p.PassengerTempSettingC,
		boolPtrToInt(p.IsClimateOn), boolPtrToInt(p.IsRearDefrosterOn), boolPtrToInt(p.IsFrontDefrosterOn),
		p.TpmsPressureFL, p.TpmsPressureFR, p.TpmsPressureRL, p.TpmsPressureRR,
		p.ShiftState, boolPtrToInt(p.SentryMode), boolPtrToInt(p.IsUserPresent), boolPtrToInt(p.ValetMode), nullIfEmpty(p.ClimateKeeperMode))
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

// CloseDriveFromLastPosition closes a drive that was abandoned - the
// vehicle stopped reporting entirely mid-drive rather than confirming
// a normal stop (see vehicle.Machine.OnUnreachable's doc comment) -
// using whatever position was last actually recorded for it, since
// there's no fresh telemetry to close it with otherwise. If not even
// one position was ever recorded, there's nothing to derive end values
// from at all; the row is still marked closed (so it stops looking
// "in progress" forever) but every end_* field stays NULL.
func (s *Store) CloseDriveFromLastPosition(driveID int64, at time.Time) error {
	var lastTimestamp string
	var odometerKm, rangeKm float64
	var batteryLevel int
	var lat, lng, idealRangeKm sql.NullFloat64
	err := s.db.QueryRow(`
		SELECT timestamp, odometer_km, battery_level, range_km, latitude, longitude,
		       (SELECT ideal_range_km FROM positions
		        WHERE drive_id = ? AND ideal_range_km IS NOT NULL
		        ORDER BY timestamp DESC LIMIT 1)
		FROM positions WHERE drive_id = ? ORDER BY timestamp DESC LIMIT 1
	`, driveID, driveID).Scan(&lastTimestamp, &odometerKm, &batteryLevel, &rangeKm, &lat, &lng, &idealRangeKm)
	if err == sql.ErrNoRows {
		_, err := s.db.Exec(`UPDATE drives SET end_time = ?, status = 'closed' WHERE id = ? AND status = 'open'`, fmtTime(at), driveID)
		if err != nil {
			return fmt.Errorf("close abandoned drive %d (no positions recorded): %w", driveID, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("find last position for abandoned drive %d: %w", driveID, err)
	}
	endTime, err := time.Parse(timeLayout, lastTimestamp)
	if err != nil {
		return fmt.Errorf("parse last position timestamp for abandoned drive %d: %w", driveID, err)
	}
	return s.CloseDrive(DriveEnd{
		DriveID: driveID, Time: endTime, OdometerKm: odometerKm, BatteryLevel: batteryLevel,
		RangeKm: rangeKm, IdealRangeKm: idealRangeKm.Float64, Lat: lat.Float64, Lng: lng.Float64,
	})
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
	var everFastCharger sql.NullInt64
	_ = s.db.QueryRow(`
		SELECT MAX(charger_power_kw), AVG(outside_temp_c), MAX(fast_charger_present) FROM charging_samples WHERE charging_session_id = ?
	`, e.ChargingSessionID).Scan(&maxPower, &avgOutside, &everFastCharger)

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
			outside_temp_avg_c = ?, cost = ?, is_dc_fast_charge = ?, status = 'closed'
		WHERE id = ?
	`, fmtTime(e.Time), e.BatteryLevel, e.RangeKm, e.IdealRangeKm,
		e.EnergyAddedKwh, energyUsed, maxPower.Float64,
		avgOutside.Float64, cost, everFastCharger.Int64, e.ChargingSessionID)
	return err
}

// CloseChargingSessionFromLastSample is CloseDriveFromLastPosition's
// counterpart for an abandoned charging session - see its doc comment
// and vehicle.Machine.OnUnreachable's. chargingEfficiency/pricePerKwh
// are passed through the same as a normal close, so an abandoned
// session's cost/energy-used estimate is computed consistently with
// every other session's.
func (s *Store) CloseChargingSessionFromLastSample(sessionID int64, at time.Time, chargingEfficiency, pricePerKwh float64) error {
	var lastTimestamp string
	var batteryLevel int
	var rangeKm, idealRangeKm, energyAdded sql.NullFloat64
	err := s.db.QueryRow(`
		SELECT timestamp, battery_level, range_km, ideal_range_km, charge_energy_added_kwh
		FROM charging_samples WHERE charging_session_id = ? ORDER BY timestamp DESC LIMIT 1
	`, sessionID).Scan(&lastTimestamp, &batteryLevel, &rangeKm, &idealRangeKm, &energyAdded)
	if err == sql.ErrNoRows {
		_, err := s.db.Exec(`UPDATE charging_sessions SET end_time = ?, status = 'closed' WHERE id = ? AND status = 'open'`, fmtTime(at), sessionID)
		if err != nil {
			return fmt.Errorf("close abandoned charging session %d (no samples recorded): %w", sessionID, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("find last sample for abandoned charging session %d: %w", sessionID, err)
	}
	endTime, err := time.Parse(timeLayout, lastTimestamp)
	if err != nil {
		return fmt.Errorf("parse last sample timestamp for abandoned charging session %d: %w", sessionID, err)
	}
	return s.CloseChargingSession(ChargeEnd{
		ChargingSessionID: sessionID, Time: endTime, BatteryLevel: batteryLevel,
		RangeKm: rangeKm.Float64, IdealRangeKm: idealRangeKm.Float64, EnergyAddedKwh: energyAdded.Float64,
		ChargingEfficiency: chargingEfficiency, PricePerKwh: pricePerKwh,
	})
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
	// StartRangeKm/EndRangeKm are the Tesla-reported "rated" range at
	// each end - RangeLostKm (start-end) is the input to a cheap
	// derived efficiency figure: how many rated-range km were consumed
	// per km actually driven (1.0 means the drive matched the EPA/WLTP
	// rating exactly; >1.0 means it drove less efficiently than rated).
	// See grafana/teslalog-efficiency.json for the same computation.
	StartRangeKm float64
	EndRangeKm   float64
	MaxSpeedKmh  float64
}

// RangeLostKm is StartRangeKm-EndRangeKm, floored at 0 (a charge or a
// range-estimate correction mid-drive can otherwise make this go
// negative, which isn't meaningful as "range lost").
func (d DriveSummary) RangeLostKm() float64 {
	lost := d.StartRangeKm - d.EndRangeKm
	if lost < 0 {
		return 0
	}
	return lost
}

// EfficiencyRatio is rated-range km lost per km actually driven, or 0
// if it can't be computed (no distance, or no range lost - e.g. a very
// short drive where the rated range estimate didn't tick down at all).
func (d DriveSummary) EfficiencyRatio() float64 {
	if d.DistanceKm <= 0 {
		return 0
	}
	return d.RangeLostKm() / d.DistanceKm
}

// ListDrives returns closed drives for a vehicle, optionally filtered
// by year (pass 0 for all years), newest first.
func (s *Store) ListDrives(vehicleID int64, year int) ([]DriveSummary, error) {
	q := `
		SELECT id, start_time, end_time, distance_km, duration_min, start_battery_level, end_battery_level,
			start_location, end_location, start_range_km, end_range_km, max_speed_kmh
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
		var distance, duration, startRange, endRange, maxSpeed sql.NullFloat64
		var startBattery, endBattery sql.NullInt64
		var startLoc, endLoc sql.NullString
		if err := rows.Scan(&d.ID, &d.StartTime, &endTime, &distance, &duration, &startBattery, &endBattery,
			&startLoc, &endLoc, &startRange, &endRange, &maxSpeed); err != nil {
			return nil, err
		}
		d.EndTime = endTime.String
		d.DistanceKm = distance.Float64
		d.DurationMin = duration.Float64
		d.StartBattery = int(startBattery.Int64)
		d.EndBattery = int(endBattery.Int64)
		d.StartLocation = startLoc.String
		d.EndLocation = endLoc.String
		d.StartRangeKm = startRange.Float64
		d.EndRangeKm = endRange.Float64
		d.MaxSpeedKmh = maxSpeed.Float64
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
	// EnergyUsedKwh is only populated if config.toml sets
	// [charging].efficiency - see ChargeEnd.ChargingEfficiency.
	EnergyUsedKwh     float64
	MaxChargerPowerKw float64
	// Cost is only populated if config.toml sets a price_per_kwh.
	Cost float64
	// StartRangeKm/EndRangeKm mirror DriveSummary's fields; used to
	// derive kWh per rated-range-km added, a rough charging-efficiency
	// figure (higher means more energy lost to heat/conversion getting
	// that range back, e.g. from cold-battery charging).
	StartRangeKm float64
	EndRangeKm   float64
	// IsDCFastCharge - see schema.go's charging_sessions.is_dc_fast_charge
	// comment. TeslaMate calls this the AC/DC "type" column.
	IsDCFastCharge bool
	// Location - see ChargeStart.Location's doc comment.
	Location string
}

// ChargeType returns "DC" or "AC" for display, matching TeslaMate's
// Charging Stats dashboard terminology.
func (c ChargeSummary) ChargeType() string {
	if c.IsDCFastCharge {
		return "DC"
	}
	return "AC"
}

// KwhPerRatedKm is charge_energy_added_kwh divided by the rated-range
// km gained during the session, or 0 if it can't be computed.
func (c ChargeSummary) KwhPerRatedKm() float64 {
	gained := c.EndRangeKm - c.StartRangeKm
	if gained <= 0 {
		return 0
	}
	return c.EnergyAddedKwh / gained
}

func (s *Store) ListCharges(vehicleID int64, year int) ([]ChargeSummary, error) {
	q := `
		SELECT id, start_time, end_time, start_battery_level, end_battery_level, charge_energy_added_kwh,
			charge_energy_used_kwh, max_charger_power_kw, cost, start_range_km, end_range_km, is_dc_fast_charge, location
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
		var energy, energyUsed, maxPower, cost, startRange, endRange sql.NullFloat64
		var isDC sql.NullInt64
		var loc sql.NullString
		if err := rows.Scan(&c.ID, &c.StartTime, &endTime, &startBattery, &endBattery, &energy,
			&energyUsed, &maxPower, &cost, &startRange, &endRange, &isDC, &loc); err != nil {
			return nil, err
		}
		c.Location = loc.String
		c.EndTime = endTime.String
		c.StartBattery = int(startBattery.Int64)
		c.EndBattery = int(endBattery.Int64)
		c.EnergyAddedKwh = energy.Float64
		c.EnergyUsedKwh = energyUsed.Float64
		c.MaxChargerPowerKw = maxPower.Float64
		c.Cost = cost.Float64
		c.StartRangeKm = startRange.Float64
		c.EndRangeKm = endRange.Float64
		c.IsDCFastCharge = isDC.Int64 != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// LifetimeStats is a handful of all-time totals cheap to show
// alongside "today" stats - the odometer reading in particular is the
// same figure TeslaMate's own vehicle status widget shows prominently.
type LifetimeStats struct {
	OdometerKm   float64
	TotalDrives  int
	TotalKm      float64
	TotalCharges int
	TotalKwh     float64
}

// Lifetime aggregates all-time totals for a vehicle. Cheap even on a
// long-running Pi: each is a single indexed aggregate query, run once
// per portal page load, not on any hot polling path.
func (s *Store) Lifetime(vehicleID int64) (LifetimeStats, error) {
	var out LifetimeStats
	var odometer sql.NullFloat64
	if err := s.db.QueryRow(`
		SELECT end_odometer_km FROM drives WHERE vehicle_id = ? AND status = 'closed' AND end_odometer_km IS NOT NULL
		ORDER BY end_time DESC LIMIT 1
	`, vehicleID).Scan(&odometer); err != nil && err != sql.ErrNoRows {
		return out, err
	}
	out.OdometerKm = odometer.Float64

	var totalKm sql.NullFloat64
	if err := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(distance_km), 0) FROM drives WHERE vehicle_id = ? AND status = 'closed'
	`, vehicleID).Scan(&out.TotalDrives, &totalKm); err != nil {
		return out, err
	}
	out.TotalKm = totalKm.Float64

	var totalKwh sql.NullFloat64
	if err := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(charge_energy_added_kwh), 0) FROM charging_sessions WHERE vehicle_id = ? AND status = 'closed'
	`, vehicleID).Scan(&out.TotalCharges, &totalKwh); err != nil {
		return out, err
	}
	out.TotalKwh = totalKwh.Float64

	return out, nil
}

// SleepStats is how a vehicle's time was split, over some window,
// between asleep-like states (asleep/offline/suspended - not being
// polled at all) and awake ones (online/idle/driving/charging). This
// is a concrete number for teslalog's own core design goal (see the
// README's Sleep behavior section): it never wakes a sleeping car, so
// an owner can see that policy actually paying off, not just trust it.
type SleepStats struct {
	WindowHours float64
	AsleepHours float64
}

// AwakeHours is WindowHours-AsleepHours - the remainder, kept as a
// method rather than a third stored field so the two can never drift
// out of sync with each other.
func (s SleepStats) AwakeHours() float64 {
	h := s.WindowHours - s.AsleepHours
	if h < 0 {
		return 0
	}
	return h
}

// AsleepPct is AsleepHours as a percentage of WindowHours, or 0 if
// there's no window (e.g. the vehicle was only just seen for the
// first time).
func (s SleepStats) AsleepPct() float64 {
	if s.WindowHours <= 0 {
		return 0
	}
	return s.AsleepHours / s.WindowHours * 100
}

func isAsleepLikeState(state string) bool {
	switch state {
	case "asleep", "offline", "suspended":
		return true
	default:
		return false
	}
}

// SleepStats24h computes SleepStats for the 24 hours ending at now.
// Each states row is clipped to that window (a state that started
// before the window, or is still open/hasn't ended, contributes only
// its overlapping portion) - this is plain interval arithmetic in Go,
// not a heavier SQL window function, since the row count per vehicle
// per day is tiny (state changes, not polls).
func (s *Store) SleepStats24h(vehicleID int64, now time.Time) (SleepStats, error) {
	windowStart := now.Add(-24 * time.Hour)
	out := SleepStats{WindowHours: 24}

	rows, err := s.db.Query(`
		SELECT state, started_at, ended_at FROM states
		WHERE vehicle_id = ? AND started_at <= ? AND (ended_at IS NULL OR ended_at >= ?)
		ORDER BY started_at
	`, vehicleID, fmtTime(now), fmtTime(windowStart))
	if err != nil {
		return out, err
	}
	defer rows.Close()

	for rows.Next() {
		var state, startedAt string
		var endedAt sql.NullString
		if err := rows.Scan(&state, &startedAt, &endedAt); err != nil {
			return out, err
		}
		start, err := time.Parse(timeLayout, startedAt)
		if err != nil {
			continue
		}
		end := now
		if endedAt.Valid {
			if e, err := time.Parse(timeLayout, endedAt.String); err == nil {
				end = e
			}
		}
		if start.Before(windowStart) {
			start = windowStart
		}
		if end.After(now) {
			end = now
		}
		if end.Before(start) {
			continue
		}
		if isAsleepLikeState(state) {
			out.AsleepHours += end.Sub(start).Hours()
		}
	}
	return out, rows.Err()
}

// LatestBatteryReading returns the most recent battery level/rated
// range/ideal range known for a vehicle, preferring an in-progress
// drive or charge (fresher than idle polling) over the last idle
// battery_samples row. ok is false if nothing has been recorded yet.
func (s *Store) LatestBatteryReading(vehicleID int64) (ok bool, level int, rangeKm, idealRangeKm float64, at string, err error) {
	if driveID, derr := s.OpenDriveID(vehicleID); derr != nil {
		return false, 0, 0, 0, "", derr
	} else if driveID != 0 {
		// ideal_range_km can genuinely be NULL on the single most recent
		// position row: streaming-derived samples (the majority of any
		// drive's rows, since streaming reports far more often than the
		// REST poll interval) don't carry it at all - see
		// PositionSample's doc comment. Falling back to the latest
		// non-null value (typically only a few seconds stale, since
		// ideal range itself barely changes moment to moment) is a far
		// better answer than erroring, or than silently reporting 0 as
		// if the reading were genuinely "no range".
		var idealRangeKmNull sql.NullFloat64
		scanErr := s.db.QueryRow(`
			SELECT battery_level, range_km, timestamp,
			       (SELECT ideal_range_km FROM positions
			        WHERE drive_id = ? AND ideal_range_km IS NOT NULL
			        ORDER BY timestamp DESC LIMIT 1)
			FROM positions WHERE drive_id = ? ORDER BY timestamp DESC LIMIT 1
		`, driveID, driveID).Scan(&level, &rangeKm, &at, &idealRangeKmNull)
		if scanErr == nil {
			return true, level, rangeKm, idealRangeKmNull.Float64, at, nil
		} else if scanErr != sql.ErrNoRows {
			return false, 0, 0, 0, "", scanErr
		}
	}

	if chargeID, cerr := s.OpenChargingSessionID(vehicleID); cerr != nil {
		return false, 0, 0, 0, "", cerr
	} else if chargeID != 0 {
		scanErr := s.db.QueryRow(`
			SELECT battery_level, range_km, ideal_range_km, timestamp FROM charging_samples
			WHERE charging_session_id = ? ORDER BY timestamp DESC LIMIT 1
		`, chargeID).Scan(&level, &rangeKm, &idealRangeKm, &at)
		if scanErr == nil {
			return true, level, rangeKm, idealRangeKm, at, nil
		} else if scanErr != sql.ErrNoRows {
			return false, 0, 0, 0, "", scanErr
		}
	}

	err = s.db.QueryRow(`
		SELECT battery_level, battery_range_km, ideal_battery_range_km, timestamp FROM battery_samples
		WHERE vehicle_id = ? ORDER BY timestamp DESC LIMIT 1
	`, vehicleID).Scan(&level, &rangeKm, &idealRangeKm, &at)
	if err == sql.ErrNoRows {
		return false, 0, 0, 0, "", nil
	}
	if err != nil {
		return false, 0, 0, 0, "", err
	}
	return true, level, rangeKm, idealRangeKm, at, nil
}
