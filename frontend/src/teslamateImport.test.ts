import initSqlJs from 'sql.js'
import { describe, expect, it } from 'vitest'
import { importTeslaMateDump, isPostgresDump } from './teslamateImport'

const tinyDump = `--
-- PostgreSQL database dump
--
COPY public.addresses (id, display_name, latitude, longitude, road, city, county, state, postcode, country) FROM stdin;
1	Home	35.1	-85.1	Lee Highway	Chattanooga	Hamilton County	Tennessee	37421	United States
\\.
COPY public.cars (id, vin, name, model, efficiency, inserted_at) FROM stdin;
1	VIN123	Roadrunner	Y	0.1314	2026-01-01 00:00:00
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

describe('TeslaMate import units and address components', () => {
  const load = async () => {
    const file = new File([tinyDump], 'teslamate.sql', { type: 'application/sql' })
    const bytes = await importTeslaMateDump(file, () => undefined)
    const SQL = await initSqlJs()
    return new SQL.Database(bytes)
  }

  // TeslaMate's cars.efficiency is kWh per km (0.1314); this column is Wh
  // per km (131.4). Imported verbatim it was 1000x too small, which
  // silently zeroed every consumption and energy figure derived from it:
  // the Overview read "Ø Consumption 0 Wh/mi" over 3,109 logged miles.
  it('converts efficiency from kWh/km to Wh/km', async () => {
    const database = await load()
    try {
      const [result] = database.exec('SELECT efficiency_wh_km FROM vehicles')
      expect(result?.values[0]?.[0]).toBeCloseTo(131.4, 4)
    } finally {
      database.close()
    }
  })

  // geocode_cache is what the Locations dashboard reads for "how many
  // cities / states / countries". TeslaMate's addresses table holds those
  // components and they were being discarded, so that dashboard failed
  // outright on any imported database.
  it('populates geocode_cache from TeslaMate addresses', async () => {
    const database = await load()
    try {
      const [result] = database.exec(
        `SELECT name, road, city, county, state, postcode, country,
                ROUND(lat_key, 4), ROUND(lng_key, 4)
         FROM geocode_cache`,
      )
      expect(result?.values).toHaveLength(1)
      expect(result?.values[0]).toEqual([
        'Home',
        'Lee Highway',
        'Chattanooga',
        'Hamilton County',
        'Tennessee',
        '37421',
        'United States',
        // Rounded to four decimals, matching geocode.roundCoord in the Go
        // daemon, so an imported database joins the same way a logged one
        // does.
        35.1,
        -85.1,
      ])
    } finally {
      database.close()
    }
  })

  // The importer's schema is generated from the Go source now; this is the
  // end-to-end proof that the tables it forgot actually exist.
  it('creates every table the viewer validates', async () => {
    const database = await load()
    try {
      const [result] = database.exec("SELECT name FROM sqlite_schema WHERE type='table'")
      const tables = new Set((result?.values ?? []).map((row) => String(row[0])))
      for (const table of [
        'vehicles', 'states', 'drives', 'positions', 'charging_sessions',
        'charging_samples', 'battery_samples', 'software_updates', 'geocode_cache',
      ]) {
        expect(tables, `missing ${table}`).toContain(table)
      }
    } finally {
      database.close()
    }
  })

  it('carries the battery_heater columns the old schema copy had dropped', async () => {
    const database = await load()
    try {
      const [result] = database.exec('PRAGMA table_info(positions)')
      const columns = new Set((result?.values ?? []).map((row) => String(row[1])))
      expect(columns).toContain('battery_heater')
      expect(columns).toContain('battery_heater_no_power')
    } finally {
      database.close()
    }
  })
})

describe('TeslaMate import file encodings', () => {
  // PowerShell's `pg_dump ... > file.sql` writes UTF-16LE with a BOM, not
  // UTF-8 - the single most common way a Windows-produced dump arrives.
  // A naive UTF-8 read sees a null after every character and recognises
  // nothing. The 344 MB real dump landed exactly this way.
  const toUtf16le = (text: string): Uint8Array => {
    const bytes = new Uint8Array(2 + text.length * 2)
    bytes[0] = 0xff // BOM
    bytes[1] = 0xfe
    for (let i = 0; i < text.length; i++) {
      const code = text.charCodeAt(i)
      bytes[2 + i * 2] = code & 0xff
      bytes[2 + i * 2 + 1] = code >> 8
    }
    return bytes
  }

  const withBomUtf8 = (text: string): Uint8Array => {
    const body = new TextEncoder().encode(text)
    const bytes = new Uint8Array(3 + body.length)
    bytes.set([0xef, 0xbb, 0xbf], 0)
    bytes.set(body, 3)
    return bytes
  }

  const importsToOneDrive = async (bytes: Uint8Array) => {
    await initSqlJs()
    const file = new File([bytes], 'teslamate.sql')
    expect(await isPostgresDump(file)).toBe(true)
    const out = await importTeslaMateDump(file, () => undefined)
    const SQL = await initSqlJs()
    const db = new SQL.Database(out)
    try {
      return db.exec('SELECT COUNT(*) FROM drives')[0]?.values[0]?.[0]
    } finally {
      db.close()
    }
  }

  it('imports a UTF-16LE dump the way PowerShell writes it', async () => {
    expect(await importsToOneDrive(toUtf16le(tinyDump))).toBe(1)
  })

  it('imports a UTF-8 dump carrying a byte-order mark', async () => {
    expect(await importsToOneDrive(withBomUtf8(tinyDump))).toBe(1)
  })

  it('still imports a plain UTF-8 dump with no mark', async () => {
    expect(await importsToOneDrive(new TextEncoder().encode(tinyDump))).toBe(1)
  })

  it('rejects a UTF-16LE custom-format dump with the plain-format hint', async () => {
    const file = new File([toUtf16le('PGDMP\u0000\u0000junk')], 'teslamate.dump')
    await expect(importTeslaMateDump(file, () => undefined)).rejects.toThrow(/--format=plain/u)
  })
})
