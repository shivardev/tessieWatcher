import initSqlJs, { type Database, type SqlValue, type Statement } from 'sql.js'
import wasmUrl from 'sql.js/dist/sql-wasm.wasm?url'

export type ImportProgress = Readonly<{
  bytesRead: number
  fileSize: number
  rowsImported: number
  table: string | null
}>

export type ImportProgressHandler = (progress: ImportProgress) => void

type Address = Readonly<{ latitude: number | null; longitude: number | null; name: string | null }>
type PositionTarget = Readonly<
  | { kind: 'drive-start'; recordId: number }
  | { kind: 'drive-end'; recordId: number }
  | { kind: 'charging'; recordId: number }
>

const normalizedSchema = `
PRAGMA foreign_keys=OFF;
CREATE TABLE schema_meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE vehicles(
 id INTEGER PRIMARY KEY,vin TEXT,tesla_id TEXT,display_name TEXT,model TEXT,trim_badging TEXT,
 marketing_name TEXT,exterior_color TEXT,wheel_type TEXT,spoiler_type TEXT,efficiency_wh_km REAL,
 firmware_version TEXT,created_at TEXT
);
CREATE TABLE states(id INTEGER PRIMARY KEY,vehicle_id INTEGER,state TEXT,started_at TEXT,ended_at TEXT);
CREATE TABLE drives(
 id INTEGER PRIMARY KEY,vehicle_id INTEGER,start_time TEXT,end_time TEXT,start_odometer_km REAL,
 end_odometer_km REAL,distance_km REAL,duration_min REAL,start_battery_level INTEGER,
 end_battery_level INTEGER,start_range_km REAL,end_range_km REAL,start_ideal_range_km REAL,
 end_ideal_range_km REAL,start_lat REAL,start_lng REAL,end_lat REAL,end_lng REAL,start_location TEXT,
 end_location TEXT,max_speed_kmh REAL,max_power_kw REAL,min_power_kw REAL,outside_temp_avg_c REAL,
 inside_temp_avg_c REAL,ascent_m REAL,descent_m REAL,status TEXT
);
CREATE TABLE positions(
 id INTEGER PRIMARY KEY,drive_id INTEGER,vehicle_id INTEGER,timestamp TEXT,latitude REAL,longitude REAL,
 speed_kmh REAL,heading REAL,elevation_m REAL,power_kw REAL,odometer_km REAL,battery_level INTEGER,
 usable_battery_level INTEGER,range_km REAL,ideal_range_km REAL,est_range_km REAL,battery_heater_on INTEGER,
 outside_temp_c REAL,inside_temp_c REAL,fan_status INTEGER,driver_temp_setting_c REAL,
 passenger_temp_setting_c REAL,is_climate_on INTEGER,is_rear_defroster_on INTEGER,
 is_front_defroster_on INTEGER,tpms_pressure_fl REAL,tpms_pressure_fr REAL,tpms_pressure_rl REAL,
 tpms_pressure_rr REAL,shift_state TEXT,sentry_mode INTEGER,is_user_present INTEGER,valet_mode INTEGER,
 climate_keeper_mode TEXT
);
CREATE TABLE charging_sessions(
 id INTEGER PRIMARY KEY,vehicle_id INTEGER,start_time TEXT,end_time TEXT,start_battery_level INTEGER,
 end_battery_level INTEGER,start_range_km REAL,end_range_km REAL,start_ideal_range_km REAL,
 end_ideal_range_km REAL,charge_energy_added_kwh REAL,charge_energy_used_kwh REAL,
 max_charger_power_kw REAL,outside_temp_avg_c REAL,cost REAL,latitude REAL,longitude REAL,
 location TEXT,is_dc_fast_charge INTEGER,status TEXT
);
CREATE TABLE charging_samples(
 id INTEGER PRIMARY KEY,charging_session_id INTEGER,vehicle_id INTEGER,timestamp TEXT,battery_level INTEGER,
 usable_battery_level INTEGER,charger_power_kw REAL,charger_voltage REAL,charger_actual_current REAL,
 charger_pilot_current INTEGER,charger_phases INTEGER,conn_charge_cable TEXT,
 fast_charger_present INTEGER,fast_charger_brand TEXT,fast_charger_type TEXT,
 charge_energy_added_kwh REAL,range_km REAL,ideal_range_km REAL,battery_heater_on INTEGER,
 not_enough_power_to_heat INTEGER,outside_temp_c REAL,charge_limit_soc INTEGER
);
CREATE TABLE battery_samples(
 id INTEGER PRIMARY KEY AUTOINCREMENT,vehicle_id INTEGER,timestamp TEXT,battery_level INTEGER,
 battery_range_km REAL,ideal_battery_range_km REAL,source TEXT
);
CREATE TABLE software_updates(
 id INTEGER PRIMARY KEY,vehicle_id INTEGER,version TEXT,status TEXT,start_time TEXT,end_time TEXT
);
INSERT INTO schema_meta(key,value) VALUES('source','TeslaMate PostgreSQL plain dump');
`

const indexes = `
CREATE INDEX idx_states_vehicle_started ON states(vehicle_id,started_at);
CREATE INDEX idx_drives_vehicle_start ON drives(vehicle_id,start_time);
CREATE INDEX idx_drives_status ON drives(vehicle_id,status);
CREATE INDEX idx_positions_drive ON positions(drive_id,timestamp);
CREATE INDEX idx_charging_sessions_vehicle_start ON charging_sessions(vehicle_id,start_time);
CREATE INDEX idx_charging_samples_session ON charging_samples(charging_session_id,timestamp);
CREATE INDEX idx_battery_samples_vehicle_ts ON battery_samples(vehicle_id,timestamp);
CREATE INDEX idx_software_updates_vehicle ON software_updates(vehicle_id,start_time);
`

const selectedTables = new Set([
  'addresses',
  'cars',
  'charges',
  'charging_processes',
  'drives',
  'geofences',
  'positions',
  'states',
  'updates',
])

const copyHeader = /^COPY (?:public\.)?([a-z_]+) \((.+)\) FROM stdin;$/u

const unescapeCopyValue = (value: string): string | null => {
  if (value === '\\N') return null
  return value.replace(/\\([\\bfnrtv])/gu, (_, escaped: string) => {
    const values: Readonly<Record<string, string>> = {
      '\\': '\\',
      b: '\b',
      f: '\f',
      n: '\n',
      r: '\r',
      t: '\t',
      v: '\v',
    }
    return values[escaped] ?? escaped
  })
}

const parseCopyRow = (line: string): readonly (string | null)[] =>
  line.split('\t').map(unescapeCopyValue)

const columnMap = (columns: readonly string[]): ReadonlyMap<string, number> =>
  new Map(columns.map((column, index) => [column.replaceAll('"', ''), index]))

const field = (
  values: readonly (string | null)[],
  columns: ReadonlyMap<string, number>,
  name: string,
): string | null => {
  const index = columns.get(name)
  return index === undefined ? null : (values[index] ?? null)
}

const numeric = (value: string | null): number | null => {
  if (value === null || value === '') return null
  const parsed = Number(value)
  return Number.isFinite(parsed) ? parsed : null
}

const integer = (value: string | null): number | null => {
  const parsed = numeric(value)
  return parsed === null ? null : Math.trunc(parsed)
}

const boolean = (value: string | null): number | null => {
  if (value === null) return null
  if (value === 't' || value === 'true' || value === '1') return 1
  if (value === 'f' || value === 'false' || value === '0') return 0
  return null
}

const sqlValues = (...values: readonly SqlValue[]): SqlValue[] => [...values]

const prepare = (database: Database, sql: string): Statement => database.prepare(sql)

const yieldToBrowser = async (): Promise<void> =>
  new Promise((resolve) => globalThis.setTimeout(resolve, 0))

export const isPostgresDump = async (file: File): Promise<boolean> => {
  const prefix = await file.slice(0, 256).text()
  return prefix.includes('PostgreSQL database dump') || prefix.startsWith('PGDMP')
}

export const importTeslaMateDump = async (
  file: File,
  onProgress: ImportProgressHandler,
): Promise<Uint8Array> => {
  const prefix = await file.slice(0, 256).text()
  if (prefix.startsWith('PGDMP'))
    throw new Error(
      'This is a custom-format PostgreSQL dump. Export TeslaMate with pg_dump --format=plain.',
    )
  if (!prefix.includes('PostgreSQL database dump'))
    throw new Error('The selected file is not a PostgreSQL plain-text dump.')

  const SQL = await initSqlJs({ locateFile: () => wasmUrl })
  const database = new SQL.Database()
  database.exec(normalizedSchema)
  const statements = {
    battery: prepare(
      database,
      `INSERT INTO battery_samples(vehicle_id,timestamp,battery_level,battery_range_km,ideal_battery_range_km,source) VALUES(?,?,?,?,?,?)`,
    ),
    charge: prepare(
      database,
      `INSERT INTO charging_samples(id,charging_session_id,vehicle_id,timestamp,battery_level,usable_battery_level,charger_power_kw,charger_voltage,charger_actual_current,charger_pilot_current,charger_phases,conn_charge_cable,fast_charger_present,fast_charger_brand,fast_charger_type,charge_energy_added_kwh,range_km,ideal_range_km,battery_heater_on,not_enough_power_to_heat,outside_temp_c,charge_limit_soc) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
    ),
    drive: prepare(
      database,
      `INSERT INTO drives(id,vehicle_id,start_time,end_time,start_odometer_km,end_odometer_km,distance_km,duration_min,start_range_km,end_range_km,start_ideal_range_km,end_ideal_range_km,start_location,end_location,max_speed_kmh,max_power_kw,min_power_kw,outside_temp_avg_c,inside_temp_avg_c,ascent_m,descent_m,status) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
    ),
    position: prepare(
      database,
      `INSERT INTO positions(id,drive_id,vehicle_id,timestamp,latitude,longitude,speed_kmh,elevation_m,power_kw,odometer_km,battery_level,usable_battery_level,range_km,ideal_range_km,est_range_km,battery_heater_on,outside_temp_c,inside_temp_c,fan_status,driver_temp_setting_c,passenger_temp_setting_c,is_climate_on,is_rear_defroster_on,is_front_defroster_on,tpms_pressure_fl,tpms_pressure_fr,tpms_pressure_rl,tpms_pressure_rr) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
    ),
    session: prepare(
      database,
      `INSERT INTO charging_sessions(id,vehicle_id,start_time,end_time,start_battery_level,end_battery_level,start_range_km,end_range_km,start_ideal_range_km,end_ideal_range_km,charge_energy_added_kwh,charge_energy_used_kwh,outside_temp_avg_c,cost,latitude,longitude,location,status) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
    ),
    state: prepare(
      database,
      `INSERT INTO states(id,vehicle_id,state,started_at,ended_at) VALUES(?,?,?,?,?)`,
    ),
    update: prepare(
      database,
      `INSERT INTO software_updates(id,vehicle_id,version,status,start_time,end_time) VALUES(?,?,?,?,?,?)`,
    ),
    vehicle: prepare(
      database,
      `INSERT INTO vehicles(id,vin,tesla_id,display_name,model,trim_badging,marketing_name,exterior_color,wheel_type,spoiler_type,efficiency_wh_km,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
    ),
  }

  const addresses = new Map<number, Address>()
  const geofences = new Map<number, string>()
  const positionTargets = new Map<number, PositionTarget[]>()
  const pendingDriveGeofences = new Map<
    number,
    Readonly<{ end: number | null; start: number | null }>
  >()
  const pendingChargeGeofences = new Map<number, number | null>()
  const lastBatteryMinute = new Map<number, string>()
  const seen = new Set<string>()
  let activeTable: string | null = null
  let activeColumns: ReadonlyMap<string, number> = new Map()
  let bytesRead = 0
  let lineNumber = 0
  let rowsImported = 0
  let rowsSinceYield = 0
  let rowsSinceCommit = 0

  const addPositionTarget = (positionId: number | null, target: PositionTarget): void => {
    if (positionId === null) return
    positionTargets.set(positionId, [...(positionTargets.get(positionId) ?? []), target])
  }

  const address = (id: number | null): Address | null =>
    id === null ? null : (addresses.get(id) ?? null)

  const runRow = (table: string, values: readonly (string | null)[]): void => {
    const get = (name: string): string | null => field(values, activeColumns, name)
    switch (table) {
      case 'addresses': {
        const id = integer(get('id'))
        if (id !== null)
          addresses.set(id, {
            latitude: numeric(get('latitude')),
            longitude: numeric(get('longitude')),
            name: get('display_name') ?? get('name'),
          })
        return
      }
      case 'cars':
        statements.vehicle.run(
          sqlValues(
            integer(get('id')),
            get('vin') ?? `teslamate-${get('id') ?? 'vehicle'}`,
            get('eid'),
            get('name') ?? 'Tesla',
            get('model'),
            get('trim_badging'),
            get('marketing_name'),
            get('exterior_color'),
            get('wheel_type'),
            get('spoiler_type'),
            numeric(get('efficiency')),
            get('inserted_at'),
          ),
        )
        return
      case 'charges': {
        const sessionId = integer(get('charging_process_id'))
        const timestamp = get('date')
        if (sessionId === null || timestamp === null) return
        statements.charge.run(
          sqlValues(
            integer(get('id')),
            sessionId,
            0,
            timestamp,
            integer(get('battery_level')),
            integer(get('usable_battery_level')),
            numeric(get('charger_power')),
            numeric(get('charger_voltage')),
            numeric(get('charger_actual_current')),
            integer(get('charger_pilot_current')),
            integer(get('charger_phases')),
            get('conn_charge_cable'),
            boolean(get('fast_charger_present')),
            get('fast_charger_brand'),
            get('fast_charger_type'),
            numeric(get('charge_energy_added')),
            numeric(get('rated_battery_range_km')),
            numeric(get('ideal_battery_range_km')),
            boolean(get('battery_heater_on')),
            boolean(get('not_enough_power_to_heat')),
            numeric(get('outside_temp')),
            null,
          ),
        )
        return
      }
      case 'charging_processes': {
        const id = integer(get('id'))
        const vehicleId = integer(get('car_id'))
        const startTime = get('start_date')
        if (id === null || vehicleId === null || startTime === null) return
        const location = address(integer(get('address_id')))
        statements.session.run(
          sqlValues(
            id,
            vehicleId,
            startTime,
            get('end_date'),
            integer(get('start_battery_level')),
            integer(get('end_battery_level')),
            numeric(get('start_rated_range_km')),
            numeric(get('end_rated_range_km')),
            numeric(get('start_ideal_range_km')),
            numeric(get('end_ideal_range_km')),
            numeric(get('charge_energy_added')),
            numeric(get('charge_energy_used')),
            numeric(get('outside_temp_avg')),
            numeric(get('cost')),
            location?.latitude ?? null,
            location?.longitude ?? null,
            location?.name ?? null,
            get('end_date') === null ? 'open' : 'closed',
          ),
        )
        addPositionTarget(integer(get('position_id')), { kind: 'charging', recordId: id })
        pendingChargeGeofences.set(id, integer(get('geofence_id')))
        return
      }
      case 'drives': {
        const id = integer(get('id'))
        const vehicleId = integer(get('car_id'))
        const startTime = get('start_date')
        if (id === null || vehicleId === null || startTime === null) return
        const startAddress = address(integer(get('start_address_id')))
        const endAddress = address(integer(get('end_address_id')))
        statements.drive.run(
          sqlValues(
            id,
            vehicleId,
            startTime,
            get('end_date'),
            numeric(get('start_km')),
            numeric(get('end_km')),
            numeric(get('distance')),
            numeric(get('duration_min')),
            numeric(get('start_rated_range_km')),
            numeric(get('end_rated_range_km')),
            numeric(get('start_ideal_range_km')),
            numeric(get('end_ideal_range_km')),
            startAddress?.name ?? null,
            endAddress?.name ?? null,
            numeric(get('speed_max')),
            numeric(get('power_max')),
            numeric(get('power_min')),
            numeric(get('outside_temp_avg')),
            numeric(get('inside_temp_avg')),
            numeric(get('ascent')),
            numeric(get('descent')),
            get('end_date') === null ? 'open' : 'closed',
          ),
        )
        addPositionTarget(integer(get('start_position_id')), { kind: 'drive-start', recordId: id })
        addPositionTarget(integer(get('end_position_id')), { kind: 'drive-end', recordId: id })
        pendingDriveGeofences.set(id, {
          start: integer(get('start_geofence_id')),
          end: integer(get('end_geofence_id')),
        })
        return
      }
      case 'geofences': {
        const id = integer(get('id'))
        const name = get('name')
        if (id !== null && name !== null) geofences.set(id, name)
        return
      }
      case 'positions': {
        const id = integer(get('id'))
        const driveId = integer(get('drive_id'))
        const vehicleId = integer(get('car_id'))
        const timestamp = get('date')
        const latitude = numeric(get('latitude'))
        const longitude = numeric(get('longitude'))
        if (id === null || vehicleId === null || timestamp === null) return
        for (const target of positionTargets.get(id) ?? []) {
          if (target.kind === 'drive-start')
            database.run(
              `UPDATE drives SET start_lat=?,start_lng=?,start_battery_level=? WHERE id=?`,
              [latitude, longitude, integer(get('battery_level')), target.recordId],
            )
          else if (target.kind === 'drive-end')
            database.run(`UPDATE drives SET end_lat=?,end_lng=?,end_battery_level=? WHERE id=?`, [
              latitude,
              longitude,
              integer(get('battery_level')),
              target.recordId,
            ])
          else
            database.run(`UPDATE charging_sessions SET latitude=?,longitude=? WHERE id=?`, [
              latitude,
              longitude,
              target.recordId,
            ])
        }
        if (driveId !== null)
          statements.position.run(
            sqlValues(
              id,
              driveId,
              vehicleId,
              timestamp,
              latitude,
              longitude,
              numeric(get('speed')),
              numeric(get('elevation')),
              numeric(get('power')),
              numeric(get('odometer')),
              integer(get('battery_level')),
              integer(get('usable_battery_level')),
              numeric(get('rated_battery_range_km')),
              numeric(get('ideal_battery_range_km')),
              numeric(get('est_battery_range_km')),
              boolean(get('battery_heater_on')),
              numeric(get('outside_temp')),
              numeric(get('inside_temp')),
              integer(get('fan_status')),
              numeric(get('driver_temp_setting')),
              numeric(get('passenger_temp_setting')),
              boolean(get('is_climate_on')),
              boolean(get('is_rear_defroster_on')),
              boolean(get('is_front_defroster_on')),
              numeric(get('tpms_pressure_fl')),
              numeric(get('tpms_pressure_fr')),
              numeric(get('tpms_pressure_rl')),
              numeric(get('tpms_pressure_rr')),
            ),
          )
        const minute = timestamp.slice(0, 16)
        if (lastBatteryMinute.get(vehicleId) !== minute) {
          lastBatteryMinute.set(vehicleId, minute)
          statements.battery.run(
            sqlValues(
              vehicleId,
              timestamp,
              integer(get('battery_level')),
              numeric(get('rated_battery_range_km')),
              numeric(get('ideal_battery_range_km')),
              'teslamate-position',
            ),
          )
        }
        return
      }
      case 'states':
        statements.state.run(
          sqlValues(
            integer(get('id')),
            integer(get('car_id')),
            get('state'),
            get('start_date'),
            get('end_date'),
          ),
        )
        return
      case 'updates':
        statements.update.run(
          sqlValues(
            integer(get('id')),
            integer(get('car_id')),
            get('version'),
            get('end_date') === null ? 'installing' : 'installed',
            get('start_date'),
            get('end_date'),
          ),
        )
    }
  }

  const processLine = async (line: string): Promise<void> => {
    lineNumber += 1
    if (activeTable === null) {
      const match = copyHeader.exec(line)
      if (!match) return
      const table = match[1]
      if (!table || !selectedTables.has(table)) return
      activeTable = table
      activeColumns = columnMap((match[2] ?? '').split(',').map((column) => column.trim()))
      seen.add(table)
      onProgress({ bytesRead, fileSize: file.size, rowsImported, table })
      return
    }
    if (line === '\\.') {
      activeTable = null
      activeColumns = new Map()
      return
    }
    try {
      runRow(activeTable, parseCopyRow(line))
    } catch (reason: unknown) {
      throw new Error(
        `Could not import ${activeTable} near dump line ${lineNumber}: ${reason instanceof Error ? reason.message : 'invalid row'}`,
      )
    }
    rowsImported += 1
    rowsSinceYield += 1
    rowsSinceCommit += 1
    if (rowsSinceCommit >= 50_000) {
      database.exec('COMMIT; BEGIN')
      rowsSinceCommit = 0
    }
    if (rowsSinceYield >= 5_000) {
      rowsSinceYield = 0
      onProgress({ bytesRead, fileSize: file.size, rowsImported, table: activeTable })
      await yieldToBrowser()
    }
  }

  try {
    database.exec('BEGIN')
    const reader = file.stream().getReader()
    const decoder = new TextDecoder()
    let buffer = ''
    while (true) {
      const chunk = await reader.read()
      if (chunk.done) break
      bytesRead += chunk.value.byteLength
      buffer += decoder.decode(chunk.value, { stream: true })
      let newline = buffer.indexOf('\n')
      while (newline >= 0) {
        const rawLine = buffer.slice(0, newline)
        buffer = buffer.slice(newline + 1)
        await processLine(rawLine.endsWith('\r') ? rawLine.slice(0, -1) : rawLine)
        newline = buffer.indexOf('\n')
      }
    }
    buffer += decoder.decode()
    if (buffer) await processLine(buffer)
    database.exec('COMMIT')

    const missing = [
      'cars',
      'drives',
      'positions',
      'charging_processes',
      'charges',
      'states',
    ].filter((table) => !seen.has(table))
    if (missing.length > 0)
      throw new Error(`TeslaMate dump is missing required COPY sections: ${missing.join(', ')}.`)

    onProgress({ bytesRead: file.size, fileSize: file.size, rowsImported, table: 'Finalizing' })
    database.exec('BEGIN')
    for (const [driveId, ids] of pendingDriveGeofences) {
      const start = ids.start === null ? null : (geofences.get(ids.start) ?? null)
      const end = ids.end === null ? null : (geofences.get(ids.end) ?? null)
      database.run(
        `UPDATE drives SET start_location=COALESCE(?,start_location),end_location=COALESCE(?,end_location) WHERE id=?`,
        [start, end, driveId],
      )
    }
    for (const [sessionId, geofenceId] of pendingChargeGeofences) {
      const location = geofenceId === null ? null : (geofences.get(geofenceId) ?? null)
      if (location !== null)
        database.run(`UPDATE charging_sessions SET location=? WHERE id=?`, [location, sessionId])
    }
    database.exec(`
      UPDATE charging_samples SET vehicle_id=COALESCE((SELECT vehicle_id FROM charging_sessions s WHERE s.id=charging_session_id),0);
      UPDATE charging_sessions SET
        max_charger_power_kw=(SELECT MAX(charger_power_kw) FROM charging_samples c WHERE c.charging_session_id=charging_sessions.id),
        is_dc_fast_charge=COALESCE((SELECT MAX(fast_charger_present) FROM charging_samples c WHERE c.charging_session_id=charging_sessions.id),0);
      UPDATE vehicles SET firmware_version=(
        SELECT version FROM software_updates u WHERE u.vehicle_id=vehicles.id ORDER BY COALESCE(u.end_time,u.start_time) DESC LIMIT 1
      );
    `)
    database.exec(indexes)
    database.exec('COMMIT')
    onProgress({ bytesRead: file.size, fileSize: file.size, rowsImported, table: null })
    return database.export()
  } catch (reason: unknown) {
    try {
      database.exec('ROLLBACK')
    } catch {
      // No active transaction remains.
    }
    throw reason
  } finally {
    Object.values(statements).forEach((statement) => statement.free())
    database.close()
  }
}
