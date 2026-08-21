package storage

// schema is applied on every startup with CREATE TABLE/INDEX IF NOT
// EXISTS statements, so it is safe to run against an existing database.
// There is intentionally no external migration framework: for a
// single-binary embedded logger, a versioned, idempotent schema string
// is easier to reason about and audit than a migrations directory.
//
// Column choices are checked directly against TeslaMate's own Ecto
// schemas (lib/teslamate/log/{car,position,drive,charging_process,
// charge,state,update}.ex) for data parity: every field TeslaMate
// records per drive/position/charge is captured here too, in metric
// units throughout.
//
// geofences/geocode_cache and drives.start_location/end_location/
// charging_sessions.location are teslalog's own, simpler take on what
// TeslaMate's geofences/locations tables do: named zones (config.toml's
// [[geofence]] entries) checked first, falling back to a cached
// reverse-geocoding lookup (internal/geocode) if enabled - see the
// README's Geofencing & locations section. Cost-by-geofence pricing is
// still not ported; charging_sessions.cost is one flat rate for the
// whole account (config.toml's [charging].price_per_kwh).
const schema = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schema_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS vehicles (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	vin            TEXT NOT NULL UNIQUE,
	tesla_id       TEXT,
	display_name   TEXT,
	-- model/trim_badging/marketing_name are derived from vehicle_config's
	-- car_type+trim_badging+VIN via the same lookup table TeslaMate uses
	-- (see internal/tesla.IdentifyVehicle) - they are NOT raw API
	-- passthrough, same as in TeslaMate's own cars table.
	model          TEXT,
	trim_badging   TEXT,
	marketing_name TEXT,
	exterior_color TEXT,
	wheel_type     TEXT,
	spoiler_type   TEXT,
	-- User-supplied estimate (Wh/km), not reported by the API. TeslaMate
	-- uses this the same way: to project range from battery %.
	efficiency_wh_km REAL,
	created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- Coarse-grained state machine history. TeslaMate's own "states" table
-- only tracks online/offline/asleep; teslalog additionally splits
-- "online" into driving/charging/idle/suspended, which TeslaMate keeps
-- only in-memory. ended_at is NULL while the state is still current.
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
	-- "Rated" range (Tesla API's battery_range) start/end.
	start_range_km        REAL,
	end_range_km          REAL,
	-- "Ideal" range, a separate (often frozen/deprecated) figure
	-- TeslaMate keeps distinct from rated range.
	start_ideal_range_km  REAL,
	end_ideal_range_km    REAL,
	start_lat             REAL,
	start_lng             REAL,
	end_lat               REAL,
	end_lng               REAL,
	-- Resolved via config.toml's [[geofence]] entries, falling back to
	-- geocode_cache/reverse-geocoding if [geocoding].enabled - NULL if
	-- neither matched (raw start_lat/start_lng are always there anyway).
	start_location        TEXT,
	end_location          TEXT,
	max_speed_kmh         REAL,
	max_power_kw          REAL,
	min_power_kw          REAL,
	outside_temp_avg_c    REAL,
	inside_temp_avg_c     REAL,
	-- Cumulative elevation gain/loss over the drive, derived from
	-- positions.elevation_m at close time (matches TeslaMate's
	-- ascent/descent, added in its 2025 migration).
	ascent_m              REAL,
	descent_m             REAL,
	status                TEXT NOT NULL DEFAULT 'open'
);
CREATE INDEX IF NOT EXISTS idx_drives_vehicle_start ON drives(vehicle_id, start_time);
CREATE INDEX IF NOT EXISTS idx_drives_status ON drives(vehicle_id, status);

-- One row per streaming/polling sample while a drive is open. Field set
-- mirrors TeslaMate's positions table field-for-field (see
-- lib/teslamate/log/position.ex), plus heading/shift_state which
-- TeslaMate tracks differently.
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
	range_km                 REAL, -- rated
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
	-- Not tracked by TeslaMate at all (it reads these live off
	-- vehicle_state only to decide sleep-safety, never persists them) -
	-- useful here for diagnosing "why won't my car sleep" after the
	-- fact, independent of any real-time decision.
	sentry_mode              INTEGER,
	is_user_present          INTEGER,
	valet_mode               INTEGER,
	-- "off"/"dog"/"camp"/"on" - also not tracked by TeslaMate.
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
	-- Estimated (not directly reported by the API - TeslaMate itself
	-- models this from added energy and a configurable efficiency
	-- factor, see config.toml's [charging].efficiency).
	charge_energy_used_kwh   REAL,
	max_charger_power_kw     REAL,
	outside_temp_avg_c       REAL,
	-- Populated only if config.toml sets a price_per_kwh; 0/NULL otherwise.
	cost                     REAL,
	latitude                 REAL,
	longitude                REAL,
	-- Same resolution as drives.start_location/end_location - see there.
	location                 TEXT,
	-- Derived at close time from whether any sample in this session had
	-- fast_charger_present set - i.e. Supercharger/CCS/CHAdeMO (DC) vs a
	-- wall connector/mobile connector (AC). TeslaMate's own dashboards
	-- surface this same AC/DC split for its "Charging Stats" panel.
	is_dc_fast_charge        INTEGER,
	status                   TEXT NOT NULL DEFAULT 'open'
);
CREATE INDEX IF NOT EXISTS idx_charging_sessions_vehicle_start ON charging_sessions(vehicle_id, start_time);
CREATE INDEX IF NOT EXISTS idx_charging_sessions_status ON charging_sessions(vehicle_id, status);

-- Field set mirrors TeslaMate's charges table (lib/teslamate/log/charge.ex).
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
	range_km                 REAL, -- rated
	ideal_range_km           REAL,
	battery_heater_on        INTEGER,
	not_enough_power_to_heat INTEGER,
	outside_temp_c           REAL,
	-- Not tracked by TeslaMate at all - distinguishes "charging stopped
	-- because it hit the limit" from "unplugged", which TeslaMate's own
	-- data can't tell apart.
	charge_limit_soc         INTEGER
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

-- Persistent cache of resolved (lat,lng) -> place name lookups, so the
-- same spot is never reverse-geocoded twice - both to respect the
-- geocoding service's rate limit and to avoid unnecessary network calls.
-- lat_key/lng_key are coordinates rounded to ~11m precision (see
-- internal/geocode.roundCoord), not the raw sample coordinates.
CREATE TABLE IF NOT EXISTS geocode_cache (
	lat_key    REAL NOT NULL,
	lng_key    REAL NOT NULL,
	name       TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	PRIMARY KEY (lat_key, lng_key)
);

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
