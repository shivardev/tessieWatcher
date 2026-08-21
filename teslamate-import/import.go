package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

// importCar upserts (by VIN) the source's first car into teslalog's
// vehicles table and returns its teslalog-side id. Upserting by VIN
// (rather than always inserting) means running this importer against
// a teslalog database that's already been logging for a while doesn't
// create a duplicate vehicle row - the imported history and whatever
// teslalog has already recorded end up under the same vehicle.
func importCar(ctx context.Context, pg *pgx.Conn, sq *sql.DB) (int64, error) {
	cols, err := tableColumns(ctx, pg, "cars")
	if err != nil {
		return 0, err
	}

	q := fmt.Sprintf(`SELECT vin, %s, %s, %s, %s, %s, %s, %s, %s, %s FROM cars ORDER BY id LIMIT 1`,
		pick(cols, "eid"), pick(cols, "name"), pick(cols, "model"), pick(cols, "trim_badging"),
		pick(cols, "marketing_name"), pick(cols, "exterior_color"), pick(cols, "wheel_type"),
		pick(cols, "spoiler_type"), pick(cols, "efficiency"))

	var vin string
	var eid *int64
	var name, model, trim, marketing, exterior, wheel, spoiler *string
	var efficiency *float64
	if err := pg.QueryRow(ctx, q).Scan(&vin, &eid, &name, &model, &trim, &marketing, &exterior, &wheel, &spoiler, &efficiency); err != nil {
		return 0, fmt.Errorf("read source car: %w", err)
	}

	var teslaID *string
	if eid != nil {
		s := strconv.FormatInt(*eid, 10)
		teslaID = &s
	}

	var vehicleID int64
	err = sq.QueryRowContext(ctx, `
		INSERT INTO vehicles (vin, tesla_id, display_name, model, trim_badging, marketing_name, exterior_color, wheel_type, spoiler_type)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(vin) DO UPDATE SET
			tesla_id=excluded.tesla_id, display_name=excluded.display_name, model=excluded.model,
			trim_badging=excluded.trim_badging, marketing_name=excluded.marketing_name,
			exterior_color=excluded.exterior_color, wheel_type=excluded.wheel_type, spoiler_type=excluded.spoiler_type
		RETURNING id
	`, vin, teslaID, name, model, trim, marketing, exterior, wheel, spoiler).Scan(&vehicleID)
	if err != nil {
		return 0, fmt.Errorf("upsert vehicle: %w", err)
	}
	if efficiency != nil {
		if _, err := sq.ExecContext(ctx, `UPDATE vehicles SET efficiency_wh_km = COALESCE(efficiency_wh_km, ?) WHERE id = ?`, *efficiency, vehicleID); err != nil {
			return 0, fmt.Errorf("set efficiency: %w", err)
		}
	}

	fmt.Println("imported car:", vin, "-> vehicle id", vehicleID)
	return vehicleID, nil
}

// importDrives imports every closed (end_date set) drive and returns a
// map from the source's drive id to teslalog's newly-assigned id, used
// by importPositions to attach positions to the right drive.
func importDrives(ctx context.Context, pg *pgx.Conn, sq *sql.DB, vehicleID int64) (map[int64]int64, error) {
	cols, err := tableColumns(ctx, pg, "drives")
	if err != nil {
		return nil, err
	}

	geofenceJoin := fmt.Sprintf("LEFT JOIN geofences gf1 ON gf1.id = %s", pickQualified(cols, "d", "start_geofence_id"))
	geofenceJoin2 := fmt.Sprintf("LEFT JOIN geofences gf2 ON gf2.id = %s", pickQualified(cols, "d", "end_geofence_id"))
	addrJoin := fmt.Sprintf("LEFT JOIN addresses a1 ON a1.id = %s", pickQualified(cols, "d", "start_address_id"))
	addrJoin2 := fmt.Sprintf("LEFT JOIN addresses a2 ON a2.id = %s", pickQualified(cols, "d", "end_address_id"))
	// drives has no start_battery_level/end_battery_level of its own -
	// confirmed against the real schema, TeslaMate derives it via
	// start_position_id/end_position_id pointing into positions, same
	// as this join does.
	posJoin := fmt.Sprintf("LEFT JOIN positions p1 ON p1.id = %s", pickQualified(cols, "d", "start_position_id"))
	posJoin2 := fmt.Sprintf("LEFT JOIN positions p2 ON p2.id = %s", pickQualified(cols, "d", "end_position_id"))

	q := fmt.Sprintf(`
		SELECT d.id, d.start_date, d.end_date, %s,
		       %s, %s, %s,
		       p1.battery_level, p2.battery_level,
		       %s, %s,
		       %s, %s,
		       %s, %s, %s,
		       %s, %s,
		       %s, %s,
		       COALESCE(gf1.name, a1.display_name), COALESCE(gf2.name, a2.display_name)
		FROM drives d
		%s
		%s
		%s
		%s
		%s
		WHERE d.end_date IS NOT NULL
		ORDER BY d.id
	`,
		pickQualified(cols, "d", "duration_min"),
		pickQualified(cols, "d", "start_km"), pickQualified(cols, "d", "end_km"), pickQualified(cols, "d", "distance"),
		pickQualified(cols, "d", "start_rated_range_km"), pickQualified(cols, "d", "end_rated_range_km"),
		pickQualified(cols, "d", "start_ideal_range_km"), pickQualified(cols, "d", "end_ideal_range_km"),
		pickQualified(cols, "d", "speed_max"), pickQualified(cols, "d", "power_max"), pickQualified(cols, "d", "power_min"),
		pickQualified(cols, "d", "outside_temp_avg"), pickQualified(cols, "d", "inside_temp_avg"),
		pickQualified(cols, "d", "ascent"), pickQualified(cols, "d", "descent"),
		geofenceJoin, geofenceJoin2, addrJoin, addrJoin2, posJoin+"\n\t\t"+posJoin2,
	)

	rows, err := pg.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query source drives: %w", err)
	}
	defer rows.Close()

	ids := map[int64]int64{}
	count := 0
	for rows.Next() {
		var srcID int64
		var startDate time.Time
		var endDate *time.Time
		var durationMin *float64
		var startKm, endKm, distance *float64
		var startBatt, endBatt *int64
		var startRated, endRated, startIdeal, endIdeal *float64
		var speedMax, powerMax, powerMin *float64
		var outsideTemp, insideTemp *float64
		var ascent, descent *float64
		var startLoc, endLoc *string

		if err := rows.Scan(&srcID, &startDate, &endDate, &durationMin, &startKm, &endKm, &distance,
			&startBatt, &endBatt, &startRated, &endRated, &startIdeal, &endIdeal,
			&speedMax, &powerMax, &powerMin, &outsideTemp, &insideTemp, &ascent, &descent,
			&startLoc, &endLoc); err != nil {
			return nil, fmt.Errorf("scan drive: %w", err)
		}

		if durationMin == nil && endDate != nil {
			d := endDate.Sub(startDate).Minutes()
			durationMin = &d
		}

		var newID int64
		err := sq.QueryRowContext(ctx, `
			INSERT INTO drives (vehicle_id, start_time, end_time, start_odometer_km, end_odometer_km,
				distance_km, duration_min, start_battery_level, end_battery_level,
				start_range_km, end_range_km, start_ideal_range_km, end_ideal_range_km,
				start_location, end_location, max_speed_kmh, max_power_kw, min_power_kw,
				outside_temp_avg_c, inside_temp_avg_c, ascent_m, descent_m, status)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'closed')
			RETURNING id
		`, vehicleID, fmtTime(startDate), fmtTimePtr(endDate), startKm, endKm, distance, durationMin,
			startBatt, endBatt, startRated, endRated, startIdeal, endIdeal,
			startLoc, endLoc, speedMax, powerMax, powerMin, outsideTemp, insideTemp, ascent, descent).Scan(&newID)
		if err != nil {
			return nil, fmt.Errorf("insert drive (source id %d): %w", srcID, err)
		}
		ids[srcID] = newID
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	fmt.Printf("imported %d drives\n", count)
	return ids, nil
}

// importPositions imports every position belonging to an imported
// (closed) drive.
func importPositions(ctx context.Context, pg *pgx.Conn, sq *sql.DB, vehicleID int64, driveIDs map[int64]int64) error {
	cols, err := tableColumns(ctx, pg, "positions")
	if err != nil {
		return err
	}

	q := fmt.Sprintf(`
		SELECT drive_id, date, latitude, longitude, %s, %s, %s, %s,
		       battery_level, %s, %s, %s, %s, %s,
		       %s, %s, %s, %s, %s, %s,
		       %s, %s, %s, %s, %s, %s
		FROM positions
		WHERE drive_id IS NOT NULL
		ORDER BY id
	`,
		pick(cols, "speed"), pick(cols, "power"), pick(cols, "odometer"), pick(cols, "elevation"),
		pick(cols, "usable_battery_level"), pick(cols, "ideal_battery_range_km"), pick(cols, "rated_battery_range_km"),
		pick(cols, "est_battery_range_km"), pick(cols, "battery_heater_on"),
		pick(cols, "outside_temp"), pick(cols, "inside_temp"), pick(cols, "fan_status"),
		pick(cols, "driver_temp_setting"), pick(cols, "passenger_temp_setting"), pick(cols, "is_climate_on"),
		pick(cols, "is_rear_defroster_on"), pick(cols, "is_front_defroster_on"),
		pick(cols, "tpms_pressure_fl"), pick(cols, "tpms_pressure_fr"), pick(cols, "tpms_pressure_rl"), pick(cols, "tpms_pressure_rr"),
	)

	rows, err := pg.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("query source positions: %w", err)
	}
	defer rows.Close()

	tx, err := sq.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO positions (drive_id, vehicle_id, timestamp, latitude, longitude, speed_kmh, power_kw,
			odometer_km, elevation_m, battery_level, usable_battery_level, ideal_range_km, range_km,
			est_range_km, battery_heater_on, outside_temp_c, inside_temp_c, fan_status,
			driver_temp_setting_c, passenger_temp_setting_c, is_climate_on,
			is_rear_defroster_on, is_front_defroster_on,
			tpms_pressure_fl, tpms_pressure_fr, tpms_pressure_rl, tpms_pressure_rr)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	count := 0
	skipped := 0
	for rows.Next() {
		var srcDriveID int64
		var date time.Time
		var lat, lng, speed, power, odo, elevation *float64
		var battLevel *int64
		var usableBatt *int64
		var idealRange, ratedRange, estRange *float64
		var battHeaterOn *bool
		var outsideTemp, insideTemp *float64
		var fanStatus *int64
		var driverTemp, passengerTemp *float64
		var isClimateOn, isRearDefrost, isFrontDefrost *bool
		var tpmsFL, tpmsFR, tpmsRL, tpmsRR *float64

		if err := rows.Scan(&srcDriveID, &date, &lat, &lng, &speed, &power, &odo, &elevation,
			&battLevel, &usableBatt, &idealRange, &ratedRange, &estRange, &battHeaterOn,
			&outsideTemp, &insideTemp, &fanStatus, &driverTemp, &passengerTemp, &isClimateOn,
			&isRearDefrost, &isFrontDefrost, &tpmsFL, &tpmsFR, &tpmsRL, &tpmsRR); err != nil {
			return fmt.Errorf("scan position: %w", err)
		}

		newDriveID, ok := driveIDs[srcDriveID]
		if !ok {
			skipped++
			continue // belongs to a drive we didn't import (still open, or otherwise excluded)
		}

		if _, err := stmt.ExecContext(ctx, newDriveID, vehicleID, fmtTime(date), lat, lng, speed, power,
			odo, elevation, battLevel, usableBatt, idealRange, ratedRange, estRange, battHeaterOn,
			outsideTemp, insideTemp, fanStatus, driverTemp, passengerTemp, isClimateOn,
			isRearDefrost, isFrontDefrost, tpmsFL, tpmsFR, tpmsRL, tpmsRR); err != nil {
			return fmt.Errorf("insert position: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("imported %d positions (%d skipped, drive not imported)\n", count, skipped)
	return nil
}

// importChargingProcesses imports every closed charging session and
// returns a source-id -> teslalog-id map for importCharges.
func importChargingProcesses(ctx context.Context, pg *pgx.Conn, sq *sql.DB, vehicleID int64) (map[int64]int64, error) {
	cols, err := tableColumns(ctx, pg, "charging_processes")
	if err != nil {
		return nil, err
	}

	geofenceJoin := fmt.Sprintf("LEFT JOIN geofences gf ON gf.id = %s", pickQualified(cols, "c", "geofence_id"))
	addrJoin := fmt.Sprintf("LEFT JOIN addresses a ON a.id = %s", pickQualified(cols, "c", "address_id"))
	// fast_charger_present lives on the per-sample "charges" table, not
	// charging_processes - is_dc_fast_charge is derived the same way
	// teslalog derives it itself at close time (see storage/schema.go's
	// comment on charging_sessions.is_dc_fast_charge).
	dcExpr := `EXISTS (SELECT 1 FROM charges ch WHERE ch.charging_process_id = c.id AND ch.fast_charger_present)`

	q := fmt.Sprintf(`
		SELECT c.id, c.start_date, c.end_date,
		       %s, %s,
		       %s, %s,
		       %s, %s,
		       %s, %s,
		       %s, %s,
		       COALESCE(gf.name, a.display_name),
		       %s
		FROM charging_processes c
		%s
		%s
		WHERE c.end_date IS NOT NULL
		ORDER BY c.id
	`,
		pickQualified(cols, "c", "start_battery_level"), pickQualified(cols, "c", "end_battery_level"),
		pickQualified(cols, "c", "start_rated_range_km"), pickQualified(cols, "c", "end_rated_range_km"),
		pickQualified(cols, "c", "start_ideal_range_km"), pickQualified(cols, "c", "end_ideal_range_km"),
		pickQualified(cols, "c", "charge_energy_added"), pickQualified(cols, "c", "charge_energy_used"),
		pickQualified(cols, "c", "outside_temp_avg"), pickQualified(cols, "c", "cost"),
		dcExpr, geofenceJoin, addrJoin,
	)

	rows, err := pg.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query source charging_processes: %w", err)
	}
	defer rows.Close()

	ids := map[int64]int64{}
	count := 0
	for rows.Next() {
		var srcID int64
		var startDate time.Time
		var endDate *time.Time
		var startBatt, endBatt *int64
		var startRated, endRated, startIdeal, endIdeal *float64
		var energyAdded, energyUsed, outsideTemp, cost *float64
		var location *string
		var isDC bool

		if err := rows.Scan(&srcID, &startDate, &endDate, &startBatt, &endBatt,
			&startRated, &endRated, &startIdeal, &endIdeal, &energyAdded, &energyUsed,
			&outsideTemp, &cost, &location, &isDC); err != nil {
			return nil, fmt.Errorf("scan charging process: %w", err)
		}

		var durationMin *float64
		if endDate != nil {
			d := endDate.Sub(startDate).Minutes()
			durationMin = &d
		}
		_ = durationMin // charging_sessions has no duration column of its own - derivable on demand, matching dashboards' own approach

		var newID int64
		err := sq.QueryRowContext(ctx, `
			INSERT INTO charging_sessions (vehicle_id, start_time, end_time, start_battery_level, end_battery_level,
				start_range_km, end_range_km, start_ideal_range_km, end_ideal_range_km,
				charge_energy_added_kwh, charge_energy_used_kwh, outside_temp_avg_c, cost, location,
				is_dc_fast_charge, status)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'closed')
			RETURNING id
		`, vehicleID, fmtTime(startDate), fmtTimePtr(endDate), startBatt, endBatt,
			startRated, endRated, startIdeal, endIdeal, energyAdded, energyUsed, outsideTemp, cost, location,
			isDC).Scan(&newID)
		if err != nil {
			return nil, fmt.Errorf("insert charging session (source id %d): %w", srcID, err)
		}
		ids[srcID] = newID
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	fmt.Printf("imported %d charging processes\n", count)
	return ids, nil
}

// importCharges imports every per-sample charges row belonging to an
// imported charging session.
func importCharges(ctx context.Context, pg *pgx.Conn, sq *sql.DB, sessionIDs map[int64]int64) error {
	cols, err := tableColumns(ctx, pg, "charges")
	if err != nil {
		return err
	}

	q := fmt.Sprintf(`
		SELECT charging_process_id, date, battery_level, %s, %s, %s, %s, %s,
		       %s, %s, %s, %s, %s, %s, %s, %s, %s
		FROM charges
		WHERE charging_process_id IS NOT NULL
		ORDER BY id
	`,
		pick(cols, "usable_battery_level"), pick(cols, "charger_power"), pick(cols, "charger_voltage"),
		pick(cols, "charger_actual_current"), pick(cols, "charger_pilot_current"),
		pick(cols, "charger_phases"), pick(cols, "conn_charge_cable"), pick(cols, "fast_charger_present"),
		pick(cols, "fast_charger_brand"), pick(cols, "fast_charger_type"), pick(cols, "charge_energy_added"),
		pick(cols, "rated_battery_range_km"), pick(cols, "ideal_battery_range_km"), pick(cols, "battery_heater_on"),
	)

	rows, err := pg.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("query source charges: %w", err)
	}
	defer rows.Close()

	tx, err := sq.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO charging_samples (charging_session_id, vehicle_id, timestamp, battery_level,
			usable_battery_level, charger_power_kw, charger_voltage, charger_actual_current,
			charger_pilot_current, charger_phases, conn_charge_cable, fast_charger_present,
			fast_charger_brand, fast_charger_type, charge_energy_added_kwh, range_km, ideal_range_km,
			battery_heater_on)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	// charging_samples.vehicle_id: look it up per-session below since
	// this table has no direct vehicle_id column of its own. Cheap
	// single-vehicle assumption is fine here (teslalog's charging_
	// sessions.vehicle_id is what actually matters for querying) - use
	// the first session's vehicle_id we can resolve.
	var vehicleID int64
	if err := sq.QueryRowContext(ctx, `SELECT vehicle_id FROM charging_sessions LIMIT 1`).Scan(&vehicleID); err != nil {
		return fmt.Errorf("resolve vehicle id for charges: %w", err)
	}

	count := 0
	skipped := 0
	for rows.Next() {
		var srcSessionID int64
		var date time.Time
		var battLevel *int64
		var usableBatt *int64
		var power, voltage, current *float64
		var pilotCurrent, phases *int64
		var cable *string
		var fastPresent *bool
		var brand, ctype *string
		var energyAdded, ratedRange, idealRange *float64
		var heaterOn *bool

		if err := rows.Scan(&srcSessionID, &date, &battLevel, &usableBatt, &power, &voltage, &current,
			&pilotCurrent, &phases, &cable, &fastPresent, &brand, &ctype, &energyAdded,
			&ratedRange, &idealRange, &heaterOn); err != nil {
			return fmt.Errorf("scan charge sample: %w", err)
		}

		newSessionID, ok := sessionIDs[srcSessionID]
		if !ok {
			skipped++
			continue
		}

		if _, err := stmt.ExecContext(ctx, newSessionID, vehicleID, fmtTime(date), battLevel, usableBatt,
			power, voltage, current, pilotCurrent, phases, cable, fastPresent, brand, ctype,
			energyAdded, ratedRange, idealRange, heaterOn); err != nil {
			return fmt.Errorf("insert charge sample: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("imported %d charge samples (%d skipped, session not imported)\n", count, skipped)
	return nil
}

// importStates imports closed state-machine entries. TeslaMate's own
// states only ever take the values online/asleep/offline, all of which
// teslalog's own richer state set (see storage/schema.go) accepts
// as-is, so no value translation is needed here (unlike
// teslamate-sync's reverse direction, which has to collapse teslalog's
// extra states back down).
func importStates(ctx context.Context, pg *pgx.Conn, sq *sql.DB, vehicleID int64) error {
	rows, err := pg.Query(ctx, `SELECT state, start_date, end_date FROM states WHERE end_date IS NOT NULL ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query source states: %w", err)
	}
	defer rows.Close()

	tx, err := sq.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO states (vehicle_id, state, started_at, ended_at) VALUES (?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	count := 0
	for rows.Next() {
		var state string
		var start time.Time
		var end *time.Time
		if err := rows.Scan(&state, &start, &end); err != nil {
			return fmt.Errorf("scan state: %w", err)
		}
		if _, err := stmt.ExecContext(ctx, vehicleID, state, fmtTime(start), fmtTimePtr(end)); err != nil {
			return fmt.Errorf("insert state: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("imported %d states\n", count)
	return nil
}

// importUpdates imports closed software update entries. TeslaMate's
// updates table tracks no status of its own (see teslamate-sync's
// schema.go comment on the same fact, from the other direction) -
// every closed (end_date set) row is, by definition, one that
// completed, so "installed" is not a guess, it's the only thing a
// closed row can mean.
func importUpdates(ctx context.Context, pg *pgx.Conn, sq *sql.DB, vehicleID int64) error {
	rows, err := pg.Query(ctx, `SELECT version, start_date, end_date FROM updates WHERE end_date IS NOT NULL ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query source updates: %w", err)
	}
	defer rows.Close()

	tx, err := sq.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO software_updates (vehicle_id, version, status, start_time, end_time) VALUES (?,?,'installed',?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	count := 0
	for rows.Next() {
		var version string
		var start time.Time
		var end *time.Time
		if err := rows.Scan(&version, &start, &end); err != nil {
			return fmt.Errorf("scan update: %w", err)
		}
		if _, err := stmt.ExecContext(ctx, vehicleID, version, fmtTime(start), fmtTimePtr(end)); err != nil {
			return fmt.Errorf("insert software update: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("imported %d software updates\n", count)
	return nil
}
