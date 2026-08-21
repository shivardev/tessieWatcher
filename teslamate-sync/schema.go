package main

// schemaSQL matches TeslaMate's real Postgres schema (column names and
// types verified live against the user's actual TeslaMate database via
// information_schema) closely enough for TeslaMate's own dashboard SQL
// to run unmodified against it. Relaxed vs the original: no foreign
// keys, no settings/geofences tables, state stored as plain text
// instead of a Postgres enum - none of that affects what TeslaMate's
// dashboard queries actually select.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS cars (
	id smallint PRIMARY KEY,
	eid bigint,
	vid bigint,
	model character varying,
	efficiency double precision,
	inserted_at timestamp without time zone DEFAULT now(),
	updated_at timestamp without time zone DEFAULT now(),
	vin text,
	name text,
	trim_badging text,
	settings_id bigint,
	exterior_color text,
	spoiler_type text,
	wheel_type text,
	display_priority smallint,
	marketing_name character varying
);

CREATE TABLE IF NOT EXISTS addresses (
	id integer PRIMARY KEY,
	display_name character varying,
	latitude numeric,
	longitude numeric,
	name character varying,
	house_number character varying,
	road character varying,
	neighbourhood character varying,
	city character varying,
	county character varying,
	postcode character varying,
	state character varying,
	state_district character varying,
	country character varying,
	raw jsonb,
	inserted_at timestamp without time zone DEFAULT now(),
	updated_at timestamp without time zone DEFAULT now(),
	osm_id bigint,
	osm_type text
);

CREATE TABLE IF NOT EXISTS drives (
	id integer PRIMARY KEY,
	start_date timestamp without time zone,
	end_date timestamp without time zone,
	outside_temp_avg numeric,
	speed_max smallint,
	power_max smallint,
	power_min smallint,
	start_ideal_range_km numeric,
	end_ideal_range_km numeric,
	start_km double precision,
	end_km double precision,
	distance double precision,
	duration_min smallint,
	car_id smallint,
	start_battery_level smallint,
	end_battery_level smallint,
	inside_temp_avg numeric,
	start_address_id integer,
	end_address_id integer,
	start_rated_range_km numeric,
	end_rated_range_km numeric,
	start_position_id integer,
	end_position_id integer,
	start_geofence_id integer,
	end_geofence_id integer,
	ascent smallint,
	descent smallint
);

CREATE TABLE IF NOT EXISTS positions (
	id integer PRIMARY KEY,
	date timestamp without time zone,
	latitude numeric,
	longitude numeric,
	speed smallint,
	power smallint,
	odometer double precision,
	ideal_battery_range_km numeric,
	battery_level smallint,
	outside_temp numeric,
	elevation smallint,
	fan_status integer,
	driver_temp_setting numeric,
	passenger_temp_setting numeric,
	is_climate_on boolean,
	is_rear_defroster_on boolean,
	is_front_defroster_on boolean,
	car_id smallint,
	drive_id integer,
	inside_temp numeric,
	battery_heater boolean,
	battery_heater_on boolean,
	battery_heater_no_power boolean,
	est_battery_range_km numeric,
	rated_battery_range_km numeric,
	usable_battery_level smallint,
	tpms_pressure_fl numeric,
	tpms_pressure_fr numeric,
	tpms_pressure_rl numeric,
	tpms_pressure_rr numeric
);

CREATE TABLE IF NOT EXISTS charging_processes (
	id integer PRIMARY KEY,
	start_date timestamp without time zone,
	end_date timestamp without time zone,
	charge_energy_added numeric,
	start_ideal_range_km numeric,
	end_ideal_range_km numeric,
	start_battery_level smallint,
	end_battery_level smallint,
	duration_min smallint,
	outside_temp_avg numeric,
	car_id smallint,
	position_id integer,
	address_id integer,
	start_rated_range_km numeric,
	end_rated_range_km numeric,
	geofence_id integer,
	charge_energy_used numeric,
	cost numeric
);

CREATE TABLE IF NOT EXISTS charges (
	id integer PRIMARY KEY,
	date timestamp without time zone,
	battery_heater_on boolean,
	battery_level smallint,
	charge_energy_added numeric,
	charger_actual_current smallint,
	charger_phases smallint,
	charger_pilot_current smallint,
	charger_power smallint,
	charger_voltage smallint,
	fast_charger_present boolean,
	conn_charge_cable character varying,
	fast_charger_brand character varying,
	fast_charger_type character varying,
	ideal_battery_range_km numeric,
	not_enough_power_to_heat boolean,
	outside_temp numeric,
	charging_process_id integer,
	battery_heater boolean,
	battery_heater_no_power boolean,
	rated_battery_range_km numeric,
	usable_battery_level smallint
);

CREATE TABLE IF NOT EXISTS states (
	id integer PRIMARY KEY,
	state text,
	start_date timestamp without time zone,
	end_date timestamp without time zone,
	car_id smallint
);

CREATE TABLE IF NOT EXISTS updates (
	id integer PRIMARY KEY,
	start_date timestamp without time zone,
	end_date timestamp without time zone,
	version character varying,
	car_id smallint
);
`
