package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// parseTimestamp converts teslalog's SQLite timestamp text (RFC3339
// with nanosecond fraction, e.g. "2026-05-11T22:54:41.281000000Z") into
// a time.Time. Needed only for rows going through pgx's CopyFrom -
// unlike the regular Exec/SendBatch calls elsewhere in this file (which
// accept the raw string directly, since pgx falls back to text-format
// parameter encoding there), CopyFrom's binary COPY protocol requires
// an actual time.Time to know how to encode a timestamp column.
func parseTimestamp(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// Every sync* function below reuses teslalog's own primary keys as the
// Postgres row's id (both are SQLite/Postgres autoincrement integers,
// so this is safe) - that means no separate ID-remapping bookkeeping
// is needed for one-to-one tables (cars, drives, positions,
// charging_processes, charges, states, updates). The one exception is
// addresses, which teslalog doesn't have as a table at all (just a
// resolved text label per drive/charge) - syncAddresses synthesizes
// one row per distinct label and hands back a name->id map that
// syncDrives/syncChargingProcesses use to fill in *_address_id.

// copySource adapts a *sql.Rows plus a per-row scan function into
// pgx.CopyFromSource, so large tables (positions, charges) stream
// straight from SQLite into Postgres's COPY protocol without buffering
// the whole result set in memory - matters once this is a car's full
// history (the stress test this session had 1M+ position rows).
type copySource struct {
	rows *sql.Rows
	scan func(*sql.Rows) ([]any, error)
	cur  []any
	err  error
}

func (c *copySource) Next() bool {
	if !c.rows.Next() {
		return false
	}
	vals, err := c.scan(c.rows)
	if err != nil {
		c.err = err
		return false
	}
	c.cur = vals
	return true
}
func (c *copySource) Values() ([]any, error) { return c.cur, c.err }
func (c *copySource) Err() error             { return c.err }

func syncCar(ctx context.Context, pg *pgx.Conn, db *sql.DB) int32 {
	var id int64
	var vin string
	var teslaID, displayName, model, trim, marketing, exterior, wheel, spoiler sql.NullString
	var efficiency sql.NullFloat64

	err := db.QueryRowContext(ctx, `
		SELECT id, vin, tesla_id, display_name, model, trim_badging, marketing_name,
		       exterior_color, wheel_type, spoiler_type, efficiency_wh_km
		FROM vehicles ORDER BY id LIMIT 1
	`).Scan(&id, &vin, &teslaID, &displayName, &model, &trim, &marketing, &exterior, &wheel, &spoiler, &efficiency)
	must(err)

	var eid *int64
	if teslaID.Valid && teslaID.String != "" {
		if v, err := strconv.ParseInt(teslaID.String, 10, 64); err == nil {
			eid = &v
		}
	}

	_, err = pg.Exec(ctx, `
		INSERT INTO cars (id, eid, vid, vin, name, model, trim_badging, marketing_name,
		                   exterior_color, wheel_type, spoiler_type, efficiency)
		VALUES ($1,$2,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	`, id, eid, vin, nullString(displayName), nullString(model), nullString(trim), nullString(marketing),
		nullString(exterior), nullString(wheel), nullString(spoiler), nullFloat(efficiency))
	must(err)

	fmt.Println("synced car:", vin)
	return int32(id)
}

// syncAddresses gathers every distinct resolved location label teslalog
// has recorded (drives.start_location/end_location, charging_sessions.
// location) and gives each one a synthetic address row, so drives/
// charges can reference start_address_id/end_address_id/address_id the
// way TeslaMate's dashboards expect - even though teslalog itself never
// modeled addresses as their own table.
func syncAddresses(ctx context.Context, pg *pgx.Conn, db *sql.DB) map[string]int32 {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT loc FROM (
			SELECT start_location AS loc FROM drives WHERE start_location IS NOT NULL AND start_location != ''
			UNION SELECT end_location FROM drives WHERE end_location IS NOT NULL AND end_location != ''
			UNION SELECT location FROM charging_sessions WHERE location IS NOT NULL AND location != ''
		) ORDER BY loc
	`)
	must(err)
	defer rows.Close()

	ids := map[string]int32{}
	var batch pgx.Batch
	next := int32(1)
	for rows.Next() {
		var name string
		must(rows.Scan(&name))
		id := next
		next++
		ids[name] = id
		batch.Queue(`INSERT INTO addresses (id, display_name, name) VALUES ($1, $2, $2)`, id, name)
	}
	must(rows.Err())

	if batch.Len() > 0 {
		must(pg.SendBatch(ctx, &batch).Close())
	}
	fmt.Printf("synced %d addresses\n", len(ids))
	return ids
}

// syncDrives syncs only closed drives (status = 'closed') - open drives
// have no end_time/end_* fields yet and TeslaMate's own drives table
// has no concept of "still in progress" (a drive only gets inserted at
// Ecto level once it ends), so this matches that convention.
func syncDrives(ctx context.Context, pg *pgx.Conn, db *sql.DB, carID int32, addr map[string]int32) map[int64]bool {
	rows, err := db.QueryContext(ctx, `
		SELECT id, start_time, end_time, start_odometer_km, end_odometer_km, distance_km, duration_min,
		       start_battery_level, end_battery_level, start_range_km, end_range_km,
		       start_ideal_range_km, end_ideal_range_km, start_location, end_location,
		       max_speed_kmh, max_power_kw, min_power_kw, outside_temp_avg_c, inside_temp_avg_c,
		       ascent_m, descent_m
		FROM drives WHERE status = 'closed'
	`)
	must(err)
	defer rows.Close()

	synced := map[int64]bool{}
	var batch pgx.Batch
	flush := func() {
		if batch.Len() > 0 {
			must(pg.SendBatch(ctx, &batch).Close())
			batch = pgx.Batch{}
		}
	}

	for rows.Next() {
		var id int64
		var startTime string
		var endTime sql.NullString
		var startOdo, endOdo, distance, duration sql.NullFloat64
		var startBatt, endBatt sql.NullInt64
		var startRange, endRange, startIdeal, endIdeal sql.NullFloat64
		var startLoc, endLoc sql.NullString
		var maxSpeed, maxPower, minPower, outsideTemp, insideTemp sql.NullFloat64
		var ascent, descent sql.NullFloat64
		must(rows.Scan(&id, &startTime, &endTime, &startOdo, &endOdo, &distance, &duration,
			&startBatt, &endBatt, &startRange, &endRange, &startIdeal, &endIdeal,
			&startLoc, &endLoc, &maxSpeed, &maxPower, &minPower, &outsideTemp, &insideTemp,
			&ascent, &descent))

		var startAddrID, endAddrID *int32
		if startLoc.Valid {
			if a, ok := addr[startLoc.String]; ok {
				startAddrID = &a
			}
		}
		if endLoc.Valid {
			if a, ok := addr[endLoc.String]; ok {
				endAddrID = &a
			}
		}

		batch.Queue(`
			INSERT INTO drives (id, start_date, end_date, start_km, end_km, distance, duration_min,
				start_battery_level, end_battery_level, start_rated_range_km, end_rated_range_km,
				start_ideal_range_km, end_ideal_range_km, start_address_id, end_address_id,
				speed_max, power_max, power_min, outside_temp_avg, inside_temp_avg, ascent, descent, car_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
		`, id, startTime, nullString(endTime), nullFloat(startOdo), nullFloat(endOdo), nullFloat(distance), nullFloat(duration),
			nullInt(startBatt), nullInt(endBatt), nullFloat(startRange), nullFloat(endRange),
			nullFloat(startIdeal), nullFloat(endIdeal), startAddrID, endAddrID,
			nullFloat(maxSpeed), nullFloat(maxPower), nullFloat(minPower), nullFloat(outsideTemp), nullFloat(insideTemp),
			nullFloat(ascent), nullFloat(descent), carID)

		synced[id] = true
		if batch.Len() >= 500 {
			flush()
		}
	}
	must(rows.Err())
	flush()

	fmt.Printf("synced %d drives\n", len(synced))
	return synced
}

// syncPositions streams every position sample belonging to a synced
// (closed) drive via Postgres's COPY protocol - the one table where
// row-at-a-time INSERTs would be too slow to be practical.
func syncPositions(ctx context.Context, pg *pgx.Conn, db *sql.DB, carID int32, closedDrives map[int64]bool) {
	rows, err := db.QueryContext(ctx, `
		SELECT p.id, p.timestamp, p.latitude, p.longitude, p.speed_kmh, p.power_kw, p.odometer_km,
		       p.ideal_range_km, p.battery_level, p.outside_temp_c, p.elevation_m, p.fan_status,
		       p.driver_temp_setting_c, p.passenger_temp_setting_c, p.is_climate_on,
		       p.is_rear_defroster_on, p.is_front_defroster_on, p.drive_id, p.inside_temp_c,
		       p.battery_heater_on, p.est_range_km, p.range_km, p.usable_battery_level,
		       p.tpms_pressure_fl, p.tpms_pressure_fr, p.tpms_pressure_rl, p.tpms_pressure_rr
		FROM positions p
		JOIN drives d ON d.id = p.drive_id
		WHERE d.status = 'closed'
		ORDER BY p.id
	`)
	must(err)
	defer rows.Close()

	columns := []string{
		"id", "date", "latitude", "longitude", "speed", "power", "odometer",
		"ideal_battery_range_km", "battery_level", "outside_temp", "elevation", "fan_status",
		"driver_temp_setting", "passenger_temp_setting", "is_climate_on",
		"is_rear_defroster_on", "is_front_defroster_on", "car_id", "drive_id", "inside_temp",
		"battery_heater", "battery_heater_on", "est_battery_range_km", "rated_battery_range_km",
		"usable_battery_level", "tpms_pressure_fl", "tpms_pressure_fr", "tpms_pressure_rl", "tpms_pressure_rr",
	}

	src := &copySource{rows: rows, scan: func(r *sql.Rows) ([]any, error) {
		var id int64
		var ts string
		var lat, lng, speed, power, odo, idealRange sql.NullFloat64
		var battLevel sql.NullInt64
		var outsideTemp sql.NullFloat64
		var elevation sql.NullFloat64
		var fanStatus sql.NullInt64
		var driverTemp, passengerTemp sql.NullFloat64
		var isClimateOn, isRearDefrost, isFrontDefrost sql.NullInt64
		var driveID int64
		var insideTemp sql.NullFloat64
		var battHeaterOn sql.NullInt64
		var estRange, ratedRange sql.NullFloat64
		var usableBatt sql.NullInt64
		var tpmsFL, tpmsFR, tpmsRL, tpmsRR sql.NullFloat64

		if err := r.Scan(&id, &ts, &lat, &lng, &speed, &power, &odo, &idealRange, &battLevel,
			&outsideTemp, &elevation, &fanStatus, &driverTemp, &passengerTemp, &isClimateOn,
			&isRearDefrost, &isFrontDefrost, &driveID, &insideTemp, &battHeaterOn, &estRange,
			&ratedRange, &usableBatt, &tpmsFL, &tpmsFR, &tpmsRL, &tpmsRR); err != nil {
			return nil, err
		}
		date, err := parseTimestamp(ts)
		if err != nil {
			return nil, err
		}

		return []any{
			id, date, nullFloat(lat), nullFloat(lng), nullFloat(speed), nullFloat(power), nullFloat(odo),
			nullFloat(idealRange), nullInt(battLevel), nullFloat(outsideTemp), nullFloat(elevation), nullInt(fanStatus),
			nullFloat(driverTemp), nullFloat(passengerTemp), nullBool(isClimateOn),
			nullBool(isRearDefrost), nullBool(isFrontDefrost), carID, driveID, nullFloat(insideTemp),
			nullBool(battHeaterOn), nullBool(battHeaterOn), nullFloat(estRange), nullFloat(ratedRange),
			nullInt(usableBatt), nullFloat(tpmsFL), nullFloat(tpmsFR), nullFloat(tpmsRL), nullFloat(tpmsRR),
		}, nil
	}}

	n, err := pg.CopyFrom(ctx, pgx.Identifier{"positions"}, columns, src)
	must(err)
	fmt.Printf("synced %d positions\n", n)
}

// syncChargingProcesses syncs only closed charging_sessions, mirroring
// syncDrives' reasoning. duration_min isn't stored by teslalog (unlike
// drives, where it's precomputed at close time) so it's derived here
// from start/end timestamps.
func syncChargingProcesses(ctx context.Context, pg *pgx.Conn, db *sql.DB, carID int32, addr map[string]int32) map[int64]bool {
	rows, err := db.QueryContext(ctx, `
		SELECT id, start_time, end_time,
		       CAST(ROUND((julianday(end_time) - julianday(start_time)) * 24 * 60) AS INTEGER) AS duration_min,
		       start_battery_level, end_battery_level, start_range_km, end_range_km,
		       start_ideal_range_km, end_ideal_range_km, charge_energy_added_kwh, charge_energy_used_kwh,
		       outside_temp_avg_c, cost, location
		FROM charging_sessions WHERE status = 'closed'
	`)
	must(err)
	defer rows.Close()

	synced := map[int64]bool{}
	var batch pgx.Batch
	flush := func() {
		if batch.Len() > 0 {
			must(pg.SendBatch(ctx, &batch).Close())
			batch = pgx.Batch{}
		}
	}

	for rows.Next() {
		var id int64
		var startTime, endTime string
		var durationMin sql.NullInt64
		var startBatt, endBatt sql.NullInt64
		var startRange, endRange, startIdeal, endIdeal sql.NullFloat64
		var energyAdded, energyUsed, outsideTemp, cost sql.NullFloat64
		var location sql.NullString
		must(rows.Scan(&id, &startTime, &endTime, &durationMin, &startBatt, &endBatt,
			&startRange, &endRange, &startIdeal, &endIdeal, &energyAdded, &energyUsed,
			&outsideTemp, &cost, &location))

		var addrID *int32
		if location.Valid {
			if a, ok := addr[location.String]; ok {
				addrID = &a
			}
		}

		batch.Queue(`
			INSERT INTO charging_processes (id, start_date, end_date, duration_min,
				start_battery_level, end_battery_level, start_rated_range_km, end_rated_range_km,
				start_ideal_range_km, end_ideal_range_km, charge_energy_added, charge_energy_used,
				outside_temp_avg, cost, address_id, car_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		`, id, startTime, endTime, nullInt(durationMin), nullInt(startBatt), nullInt(endBatt),
			nullFloat(startRange), nullFloat(endRange), nullFloat(startIdeal), nullFloat(endIdeal),
			nullFloat(energyAdded), nullFloat(energyUsed), nullFloat(outsideTemp), nullFloat(cost), addrID, carID)

		synced[id] = true
		if batch.Len() >= 500 {
			flush()
		}
	}
	must(rows.Err())
	flush()

	fmt.Printf("synced %d charging processes\n", len(synced))
	return synced
}

// syncCharges streams every charging_samples row belonging to a synced
// (closed) charging_processes row - same COPY approach as syncPositions.
func syncCharges(ctx context.Context, pg *pgx.Conn, db *sql.DB, closedSessions map[int64]bool) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, s.timestamp, s.battery_heater_on, s.battery_level, s.charge_energy_added_kwh,
		       s.charger_actual_current, s.charger_phases, s.charger_pilot_current, s.charger_power_kw,
		       s.charger_voltage, s.fast_charger_present, s.conn_charge_cable, s.fast_charger_brand,
		       s.fast_charger_type, s.ideal_range_km, s.not_enough_power_to_heat, s.outside_temp_c,
		       s.charging_session_id, s.range_km, s.usable_battery_level
		FROM charging_samples s
		JOIN charging_sessions c ON c.id = s.charging_session_id
		WHERE c.status = 'closed'
		ORDER BY s.id
	`)
	must(err)
	defer rows.Close()

	columns := []string{
		"id", "date", "battery_heater_on", "battery_level", "charge_energy_added",
		"charger_actual_current", "charger_phases", "charger_pilot_current", "charger_power",
		"charger_voltage", "fast_charger_present", "conn_charge_cable", "fast_charger_brand",
		"fast_charger_type", "ideal_battery_range_km", "not_enough_power_to_heat", "outside_temp",
		"charging_process_id", "rated_battery_range_km", "usable_battery_level", "battery_heater",
	}

	src := &copySource{rows: rows, scan: func(r *sql.Rows) ([]any, error) {
		var id int64
		var ts string
		var heaterOn sql.NullInt64
		var battLevel sql.NullInt64
		var energyAdded sql.NullFloat64
		var actualCurrent, phases, pilotCurrent, power, voltage sql.NullInt64
		var fastPresent sql.NullInt64
		var cable, brand, ctype sql.NullString
		var idealRange sql.NullFloat64
		var notEnoughPower sql.NullInt64
		var outsideTemp sql.NullFloat64
		var sessionID int64
		var ratedRange sql.NullFloat64
		var usableBatt sql.NullInt64

		if err := r.Scan(&id, &ts, &heaterOn, &battLevel, &energyAdded, &actualCurrent, &phases,
			&pilotCurrent, &power, &voltage, &fastPresent, &cable, &brand, &ctype, &idealRange,
			&notEnoughPower, &outsideTemp, &sessionID, &ratedRange, &usableBatt); err != nil {
			return nil, err
		}
		date, err := parseTimestamp(ts)
		if err != nil {
			return nil, err
		}

		return []any{
			id, date, nullBool(heaterOn), nullInt(battLevel), nullFloat(energyAdded),
			nullInt(actualCurrent), nullInt(phases), nullInt(pilotCurrent), nullInt(power),
			nullInt(voltage), nullBool(fastPresent), nullString(cable), nullString(brand),
			nullString(ctype), nullFloat(idealRange), nullBool(notEnoughPower), nullFloat(outsideTemp),
			sessionID, nullFloat(ratedRange), nullInt(usableBatt), nullBool(heaterOn),
		}, nil
	}}

	n, err := pg.CopyFrom(ctx, pgx.Identifier{"charges"}, columns, src)
	must(err)
	fmt.Printf("synced %d charges\n", n)
}

// syncStates syncs closed state-machine entries, collapsing teslalog's
// finer-grained states (driving/charging/idle/suspended - see
// storage/schema.go's comment on why teslalog splits these out) down
// to the three TeslaMate's own states enum/dashboards actually know
// about (online/asleep/offline), so TeslaMate's real Timeline/state
// panels get sensible values instead of ones they don't recognize.
func syncStates(ctx context.Context, pg *pgx.Conn, db *sql.DB, carID int32) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, state, started_at, ended_at FROM states WHERE ended_at IS NOT NULL
	`)
	must(err)
	defer rows.Close()

	var batch pgx.Batch
	count := 0
	flush := func() {
		if batch.Len() > 0 {
			must(pg.SendBatch(ctx, &batch).Close())
			batch = pgx.Batch{}
		}
	}
	for rows.Next() {
		var id int64
		var state, startedAt, endedAt string
		must(rows.Scan(&id, &state, &startedAt, &endedAt))

		switch state {
		case "driving", "charging", "idle", "suspended":
			state = "online"
		case "asleep", "offline":
			// already matches
		default:
			state = "online"
		}

		batch.Queue(`INSERT INTO states (id, state, start_date, end_date, car_id) VALUES ($1,$2,$3,$4,$5)`,
			id, state, startedAt, endedAt, carID)
		count++
		if batch.Len() >= 500 {
			flush()
		}
	}
	must(rows.Err())
	flush()
	fmt.Printf("synced %d states\n", count)
}

func syncUpdates(ctx context.Context, pg *pgx.Conn, db *sql.DB, carID int32) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, version, start_time, end_time FROM software_updates WHERE end_time IS NOT NULL
	`)
	must(err)
	defer rows.Close()

	var batch pgx.Batch
	count := 0
	for rows.Next() {
		var id int64
		var version, startTime, endTime string
		must(rows.Scan(&id, &version, &startTime, &endTime))
		batch.Queue(`INSERT INTO updates (id, version, start_date, end_date, car_id) VALUES ($1,$2,$3,$4,$5)`,
			id, version, startTime, endTime, carID)
		count++
	}
	must(rows.Err())
	if batch.Len() > 0 {
		must(pg.SendBatch(ctx, &batch).Close())
	}
	fmt.Printf("synced %d updates\n", count)
}
