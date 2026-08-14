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
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
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

	return &Store{db: db}, nil
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

// ---- vehicles ----

// UpsertVehicle ensures a vehicle row exists for vin and returns its id.
func (s *Store) UpsertVehicle(vin, teslaID, displayName, model, trim string) (int64, error) {
	_, err := s.db.Exec(`
		INSERT INTO vehicles (vin, tesla_id, display_name, model, trim_badging)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(vin) DO UPDATE SET
			tesla_id = excluded.tesla_id,
			display_name = excluded.display_name,
			model = excluded.model,
			trim_badging = excluded.trim_badging
	`, vin, teslaID, displayName, model, trim)
	if err != nil {
		return 0, fmt.Errorf("upsert vehicle: %w", err)
	}

	var id int64
	if err := s.db.QueryRow(`SELECT id FROM vehicles WHERE vin = ?`, vin).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup vehicle id: %w", err)
	}
	return id, nil
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
	Lat, Lng     float64
}

// OpenDrive inserts a new open drive row and returns its id.
func (s *Store) OpenDrive(d DriveStart) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO drives (
			vehicle_id, start_time, start_odometer_km, start_battery_level,
			start_range_km, start_lat, start_lng, status
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'open')
	`, d.VehicleID, fmtTime(d.Time), d.OdometerKm, d.BatteryLevel, d.RangeKm, d.Lat, d.Lng)
	if err != nil {
		return 0, fmt.Errorf("open drive: %w", err)
	}
	return res.LastInsertId()
}

type PositionSample struct {
	DriveID      int64
	VehicleID    int64
	Time         time.Time
	Lat, Lng     float64
	SpeedKmh     float64
	Heading      float64
	ElevationM   float64
	PowerKw      float64
	OdometerKm   float64
	BatteryLevel int
	RangeKm      float64
	ShiftState   string
}

// AppendPosition records one GPS/telemetry sample for an open drive.
func (s *Store) AppendPosition(p PositionSample) error {
	_, err := s.db.Exec(`
		INSERT INTO positions (
			drive_id, vehicle_id, timestamp, latitude, longitude, speed_kmh,
			heading, elevation_m, power_kw, odometer_km, battery_level, range_km, shift_state
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.DriveID, p.VehicleID, fmtTime(p.Time), p.Lat, p.Lng, p.SpeedKmh,
		p.Heading, p.ElevationM, p.PowerKw, p.OdometerKm, p.BatteryLevel, p.RangeKm, p.ShiftState)
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
	Lat, Lng     float64
}

// CloseDrive finalizes a drive: sets end fields, computed distance and
// duration, and marks status 'closed'. Distance/duration/max speed are
// derived from the recorded positions plus start/end odometer so the
// numbers are correct even if some samples were missed.
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

	var maxSpeed sql.NullFloat64
	_ = s.db.QueryRow(`SELECT MAX(speed_kmh) FROM positions WHERE drive_id = ?`, e.DriveID).Scan(&maxSpeed)

	_, err = s.db.Exec(`
		UPDATE drives SET
			end_time = ?, end_odometer_km = ?, end_battery_level = ?,
			end_range_km = ?, end_lat = ?, end_lng = ?,
			distance_km = ?, duration_min = ?, max_speed_kmh = ?, status = 'closed'
		WHERE id = ?
	`, fmtTime(e.Time), e.OdometerKm, e.BatteryLevel, e.RangeKm, e.Lat, e.Lng,
		distance, duration, maxSpeed.Float64, e.DriveID)
	if err != nil {
		return fmt.Errorf("close drive %d: %w", e.DriveID, err)
	}
	return nil
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
	Lat, Lng     float64
}

func (s *Store) OpenChargingSession(c ChargeStart) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO charging_sessions (
			vehicle_id, start_time, start_battery_level, start_range_km, latitude, longitude, status
		) VALUES (?, ?, ?, ?, ?, ?, 'open')
	`, c.VehicleID, fmtTime(c.Time), c.BatteryLevel, c.RangeKm, c.Lat, c.Lng)
	if err != nil {
		return 0, fmt.Errorf("open charging session: %w", err)
	}
	return res.LastInsertId()
}

type ChargingSample struct {
	ChargingSessionID int64
	VehicleID         int64
	Time              time.Time
	BatteryLevel      int
	ChargerPowerKw    float64
	ChargerVoltage    float64
	ChargerCurrent    float64
	EnergyAddedKwh    float64
	RangeKm           float64
}

func (s *Store) AppendChargingSample(c ChargingSample) error {
	_, err := s.db.Exec(`
		INSERT INTO charging_samples (
			charging_session_id, vehicle_id, timestamp, battery_level,
			charger_power_kw, charger_voltage, charger_actual_current,
			charge_energy_added_kwh, range_km
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, c.ChargingSessionID, c.VehicleID, fmtTime(c.Time), c.BatteryLevel,
		c.ChargerPowerKw, c.ChargerVoltage, c.ChargerCurrent, c.EnergyAddedKwh, c.RangeKm)
	return err
}

type ChargeEnd struct {
	ChargingSessionID int64
	Time              time.Time
	BatteryLevel      int
	RangeKm           float64
	EnergyAddedKwh    float64
}

func (s *Store) CloseChargingSession(e ChargeEnd) error {
	var maxPower sql.NullFloat64
	_ = s.db.QueryRow(`
		SELECT MAX(charger_power_kw) FROM charging_samples WHERE charging_session_id = ?
	`, e.ChargingSessionID).Scan(&maxPower)

	_, err := s.db.Exec(`
		UPDATE charging_sessions SET
			end_time = ?, end_battery_level = ?, end_range_km = ?,
			charge_energy_added_kwh = ?, max_charger_power_kw = ?, status = 'closed'
		WHERE id = ?
	`, fmtTime(e.Time), e.BatteryLevel, e.RangeKm, e.EnergyAddedKwh, maxPower.Float64, e.ChargingSessionID)
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

// ---- export/reporting helpers ----

type DriveSummary struct {
	ID           int64
	StartTime    string
	EndTime      string
	DistanceKm   float64
	DurationMin  float64
	StartBattery int
	EndBattery   int
}

// ListDrives returns closed drives for a vehicle, optionally filtered
// by year (pass 0 for all years), newest first.
func (s *Store) ListDrives(vehicleID int64, year int) ([]DriveSummary, error) {
	q := `
		SELECT id, start_time, end_time, distance_km, duration_min, start_battery_level, end_battery_level
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
		if err := rows.Scan(&d.ID, &d.StartTime, &endTime, &distance, &duration, &startBattery, &endBattery); err != nil {
			return nil, err
		}
		d.EndTime = endTime.String
		d.DistanceKm = distance.Float64
		d.DurationMin = duration.Float64
		d.StartBattery = int(startBattery.Int64)
		d.EndBattery = int(endBattery.Int64)
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
}

func (s *Store) ListCharges(vehicleID int64, year int) ([]ChargeSummary, error) {
	q := `
		SELECT id, start_time, end_time, start_battery_level, end_battery_level, charge_energy_added_kwh
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
		if err := rows.Scan(&c.ID, &c.StartTime, &endTime, &startBattery, &endBattery, &energy); err != nil {
			return nil, err
		}
		c.EndTime = endTime.String
		c.StartBattery = int(startBattery.Int64)
		c.EndBattery = int(endBattery.Int64)
		c.EnergyAddedKwh = energy.Float64
		out = append(out, c)
	}
	return out, rows.Err()
}
