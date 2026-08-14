package storage

// schema is applied on every startup with CREATE TABLE/INDEX IF NOT
// EXISTS statements, so it is safe to run against an existing database.
// There is intentionally no external migration framework: for a
// single-binary embedded logger, a versioned, idempotent schema string
// is easier to reason about and audit than a migrations directory.
const schema = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schema_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS vehicles (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	vin          TEXT NOT NULL UNIQUE,
	tesla_id     TEXT,
	display_name TEXT,
	model        TEXT,
	trim_badging TEXT,
	created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- Coarse-grained state machine history: one row per ASLEEP / ONLINE /
-- DRIVING / CHARGING / IDLE / SUSPENDED sojourn. ended_at is NULL while
-- the state is still current.
CREATE TABLE IF NOT EXISTS states (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	vehicle_id INTEGER NOT NULL REFERENCES vehicles(id),
	state      TEXT NOT NULL,
	started_at TEXT NOT NULL,
	ended_at   TEXT
);
CREATE INDEX IF NOT EXISTS idx_states_vehicle_started ON states(vehicle_id, started_at);
CREATE INDEX IF NOT EXISTS idx_states_open ON states(vehicle_id, ended_at);

CREATE TABLE IF NOT EXISTS drives (
	id                  INTEGER PRIMARY KEY AUTOINCREMENT,
	vehicle_id          INTEGER NOT NULL REFERENCES vehicles(id),
	start_time          TEXT NOT NULL,
	end_time            TEXT,
	start_odometer_km   REAL,
	end_odometer_km     REAL,
	distance_km         REAL,
	duration_min        REAL,
	start_battery_level INTEGER,
	end_battery_level   INTEGER,
	start_range_km      REAL,
	end_range_km        REAL,
	start_lat           REAL,
	start_lng           REAL,
	end_lat             REAL,
	end_lng             REAL,
	max_speed_kmh        REAL,
	status              TEXT NOT NULL DEFAULT 'open'
);
CREATE INDEX IF NOT EXISTS idx_drives_vehicle_start ON drives(vehicle_id, start_time);
CREATE INDEX IF NOT EXISTS idx_drives_status ON drives(vehicle_id, status);

-- One row per streaming/polling sample while a drive is open.
CREATE TABLE IF NOT EXISTS positions (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	drive_id       INTEGER NOT NULL REFERENCES drives(id),
	vehicle_id     INTEGER NOT NULL REFERENCES vehicles(id),
	timestamp      TEXT NOT NULL,
	latitude       REAL,
	longitude      REAL,
	speed_kmh      REAL,
	heading        REAL,
	elevation_m    REAL,
	power_kw       REAL,
	odometer_km    REAL,
	battery_level  INTEGER,
	range_km       REAL,
	shift_state    TEXT
);
CREATE INDEX IF NOT EXISTS idx_positions_drive ON positions(drive_id, timestamp);

CREATE TABLE IF NOT EXISTS charging_sessions (
	id                       INTEGER PRIMARY KEY AUTOINCREMENT,
	vehicle_id               INTEGER NOT NULL REFERENCES vehicles(id),
	start_time               TEXT NOT NULL,
	end_time                 TEXT,
	start_battery_level      INTEGER,
	end_battery_level        INTEGER,
	start_range_km           REAL,
	end_range_km             REAL,
	charge_energy_added_kwh  REAL,
	max_charger_power_kw     REAL,
	latitude                 REAL,
	longitude                REAL,
	status                   TEXT NOT NULL DEFAULT 'open'
);
CREATE INDEX IF NOT EXISTS idx_charging_sessions_vehicle_start ON charging_sessions(vehicle_id, start_time);
CREATE INDEX IF NOT EXISTS idx_charging_sessions_status ON charging_sessions(vehicle_id, status);

CREATE TABLE IF NOT EXISTS charging_samples (
	id                      INTEGER PRIMARY KEY AUTOINCREMENT,
	charging_session_id     INTEGER NOT NULL REFERENCES charging_sessions(id),
	vehicle_id              INTEGER NOT NULL REFERENCES vehicles(id),
	timestamp               TEXT NOT NULL,
	battery_level           INTEGER,
	charger_power_kw        REAL,
	charger_voltage         REAL,
	charger_actual_current  REAL,
	charge_energy_added_kwh REAL,
	range_km                REAL
);
CREATE INDEX IF NOT EXISTS idx_charging_samples_session ON charging_samples(charging_session_id, timestamp);

-- Battery/range snapshots taken opportunistically (idle polls), useful
-- for phantom-drain analysis independent of drives/charges.
CREATE TABLE IF NOT EXISTS battery_samples (
	id                     INTEGER PRIMARY KEY AUTOINCREMENT,
	vehicle_id             INTEGER NOT NULL REFERENCES vehicles(id),
	timestamp              TEXT NOT NULL,
	battery_level          INTEGER,
	battery_range_km       REAL,
	ideal_battery_range_km REAL,
	source                 TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_battery_samples_vehicle_ts ON battery_samples(vehicle_id, timestamp);

CREATE TABLE IF NOT EXISTS software_updates (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	vehicle_id INTEGER NOT NULL REFERENCES vehicles(id),
	version    TEXT NOT NULL,
	status     TEXT NOT NULL,
	start_time TEXT NOT NULL,
	end_time   TEXT
);
CREATE INDEX IF NOT EXISTS idx_software_updates_vehicle ON software_updates(vehicle_id, start_time);
`
