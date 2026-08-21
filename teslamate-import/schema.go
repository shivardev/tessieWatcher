package main

// sqliteSchema is a copy of internal/storage/schema.go's schema, kept
// in sync by hand (this tool is a separate Go module specifically so
// pgx/Postgres never becomes a dependency of teslalog's own binary -
// see teslamate-sync/README.md for the same reasoning in the other
// direction - so it can't just import internal/storage directly).
// Applied with CREATE TABLE IF NOT EXISTS, so running this importer
// against an existing, already-running teslalog database is safe: it
// never touches a row teslalog itself created, and the two can share
// one file (new rows get IDs past whatever teslalog has already used,
// since AUTOINCREMENT tracks the high-water mark for the table).
const sqliteSchema = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS vehicles (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	vin            TEXT NOT NULL UNIQUE,
	tesla_id       TEXT,
	display_name   TEXT,
	model          TEXT,
	trim_badging   TEXT,
	marketing_name TEXT,
	exterior_color TEXT,
	wheel_type     TEXT,
	spoiler_type   TEXT,
	efficiency_wh_km REAL,
	firmware_version TEXT,
	created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

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
	id                    INTEGER PRIMARY KEY AUTOINCREMENT,
	vehicle_id            INTEGER NOT NULL REFERENCES vehicles(id),
	start_time            TEXT NOT NULL,
	end_time              TEXT,
	start_odometer_km     REAL,
	end_odometer_km       REAL,
	distance_km           REAL,
	duration_min          REAL,
	start_battery_level   INTEGER,
	end_battery_level     INTEGER,
	start_range_km        REAL,
	end_range_km          REAL,
	start_ideal_range_km  REAL,
	end_ideal_range_km    REAL,
	start_lat             REAL,
	start_lng             REAL,
	end_lat               REAL,
	end_lng               REAL,
	start_location        TEXT,
	end_location          TEXT,
	max_speed_kmh         REAL,
	max_power_kw          REAL,
	min_power_kw          REAL,
	outside_temp_avg_c    REAL,
	inside_temp_avg_c     REAL,
	ascent_m              REAL,
	descent_m             REAL,
	status                TEXT NOT NULL DEFAULT 'open'
);
CREATE INDEX IF NOT EXISTS idx_drives_vehicle_start ON drives(vehicle_id, start_time);
CREATE INDEX IF NOT EXISTS idx_drives_status ON drives(vehicle_id, status);

CREATE TABLE IF NOT EXISTS positions (
	id                       INTEGER PRIMARY KEY AUTOINCREMENT,
	drive_id                 INTEGER NOT NULL REFERENCES drives(id),
	vehicle_id               INTEGER NOT NULL REFERENCES vehicles(id),
	timestamp                TEXT NOT NULL,
	latitude                 REAL,
	longitude                REAL,
	speed_kmh                REAL,
	heading                  REAL,
	elevation_m              REAL,
	power_kw                 REAL,
	odometer_km              REAL,
	battery_level            INTEGER,
	usable_battery_level     INTEGER,
	range_km                 REAL,
	ideal_range_km           REAL,
	est_range_km             REAL,
	battery_heater_on        INTEGER,
	outside_temp_c           REAL,
	inside_temp_c            REAL,
	fan_status               INTEGER,
	driver_temp_setting_c    REAL,
	passenger_temp_setting_c REAL,
	is_climate_on            INTEGER,
	is_rear_defroster_on     INTEGER,
	is_front_defroster_on    INTEGER,
	tpms_pressure_fl         REAL,
	tpms_pressure_fr         REAL,
	tpms_pressure_rl         REAL,
	tpms_pressure_rr         REAL,
	shift_state              TEXT,
	sentry_mode              INTEGER,
	is_user_present          INTEGER,
	valet_mode               INTEGER,
	climate_keeper_mode      TEXT
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
	start_ideal_range_km     REAL,
	end_ideal_range_km      REAL,
	charge_energy_added_kwh  REAL,
	charge_energy_used_kwh   REAL,
	max_charger_power_kw     REAL,
	outside_temp_avg_c       REAL,
	cost                     REAL,
	latitude                 REAL,
	longitude                REAL,
	location                 TEXT,
	is_dc_fast_charge        INTEGER,
	status                   TEXT NOT NULL DEFAULT 'open'
);
CREATE INDEX IF NOT EXISTS idx_charging_sessions_vehicle_start ON charging_sessions(vehicle_id, start_time);
CREATE INDEX IF NOT EXISTS idx_charging_sessions_status ON charging_sessions(vehicle_id, status);

CREATE TABLE IF NOT EXISTS charging_samples (
	id                       INTEGER PRIMARY KEY AUTOINCREMENT,
	charging_session_id      INTEGER NOT NULL REFERENCES charging_sessions(id),
	vehicle_id               INTEGER NOT NULL REFERENCES vehicles(id),
	timestamp                TEXT NOT NULL,
	battery_level            INTEGER,
	usable_battery_level     INTEGER,
	charger_power_kw         REAL,
	charger_voltage          REAL,
	charger_actual_current   REAL,
	charger_pilot_current    INTEGER,
	charger_phases           INTEGER,
	conn_charge_cable        TEXT,
	fast_charger_present     INTEGER,
	fast_charger_brand       TEXT,
	fast_charger_type        TEXT,
	charge_energy_added_kwh  REAL,
	range_km                 REAL,
	ideal_range_km           REAL,
	battery_heater_on        INTEGER,
	not_enough_power_to_heat INTEGER,
	outside_temp_c           REAL,
	charge_limit_soc         INTEGER
);
CREATE INDEX IF NOT EXISTS idx_charging_samples_session ON charging_samples(charging_session_id, timestamp);

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
