import initSqlJs from 'sql.js'
import { describe, expect, it } from 'vitest'
import { importTeslaMateDump, isPostgresDump } from './teslamateImport'

const tinyDump = `--
-- PostgreSQL database dump
--
COPY public.addresses (id, display_name, latitude, longitude) FROM stdin;
1	Home	35.1	-85.1
\\.
COPY public.cars (id, vin, name, model, efficiency, inserted_at) FROM stdin;
1	VIN123	Roadrunner	Y	150	2026-01-01 00:00:00
\\.
COPY public.charges (id, date, battery_level, charger_power, charging_process_id, rated_battery_range_km) FROM stdin;
1	2026-01-02 01:05:00	70	11	1	400
\\.
COPY public.charging_processes (id, start_date, end_date, car_id, address_id, charge_energy_added, start_battery_level, end_battery_level, geofence_id) FROM stdin;
1	2026-01-02 01:00:00	2026-01-02 02:00:00	1	1	20	60	80	2
\\.
COPY public.geofences (id, name) FROM stdin;
2	Home charger
\\.
COPY public.drives (id, start_date, end_date, start_km, end_km, distance, duration_min, car_id, start_address_id, end_address_id, start_position_id, end_position_id) FROM stdin;
1	2026-01-03 01:00:00	2026-01-03 01:30:00	100	120	20	30	1	1	1	1	1
\\.
COPY public.positions (id, date, latitude, longitude, speed, power, odometer, battery_level, rated_battery_range_km, car_id, drive_id) FROM stdin;
1	2026-01-03 01:00:00	35.1	-85.1	80	20	100	80	420	1	1
\\.
COPY public.states (id, state, start_date, end_date, car_id) FROM stdin;
1	online	2026-01-01 00:00:00	\\N	1
\\.
`

describe('TeslaMate PostgreSQL dump import', () => {
  it('detects and normalizes a plain COPY dump without retaining private tables', async () => {
    await initSqlJs()
    const file = new File([tinyDump], 'teslamate.sql', { type: 'application/sql' })
    expect(await isPostgresDump(file)).toBe(true)
    const bytes = await importTeslaMateDump(file, () => undefined)
    const SQL = await initSqlJs()
    const database = new SQL.Database(bytes)
    try {
      const counts = database.exec(
        `SELECT (SELECT COUNT(*) FROM vehicles),(SELECT COUNT(*) FROM drives),(SELECT COUNT(*) FROM positions),(SELECT COUNT(*) FROM charging_sessions),(SELECT COUNT(*) FROM charging_samples)`,
      )[0]?.values[0]
      expect(counts).toEqual([1, 1, 1, 1, 1])
      expect(
        database.exec(`SELECT start_location,end_location,start_lat FROM drives`)[0]?.values[0],
      ).toEqual(['Home', 'Home', 35.1])
      expect(database.exec(`SELECT location FROM charging_sessions`)[0]?.values[0]).toEqual([
        'Home charger',
      ])
      expect(database.exec(`SELECT name FROM sqlite_schema WHERE name='tokens'`)).toEqual([])
    } finally {
      database.close()
    }
  })
})
