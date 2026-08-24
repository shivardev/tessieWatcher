import initSqlJs, { type Database, type SqlValue } from 'sql.js'
import wasmUrl from 'sql.js/dist/sql-wasm.wasm?url'
import {
  chargeRowSchema,
  driveRowSchema,
  queryValueSchema,
  vehicleSchema,
  type Destination,
  type LoadedDatabase,
  type Metric,
  type QueryResult,
} from './domain'
import type { TimeRange } from './viewSettings'
const required = [
  'vehicles',
  'states',
  'drives',
  'positions',
  'charging_sessions',
  'charging_samples',
  'battery_samples',
  'software_updates',
] as const
const requiredColumns: Readonly<Record<string, readonly string[]>> = {
  vehicles: [
    'id',
    'display_name',
    'model',
    'marketing_name',
    'firmware_version',
    'efficiency_wh_km',
  ],
  states: ['vehicle_id', 'state', 'started_at', 'ended_at'],
  drives: [
    'id',
    'vehicle_id',
    'start_time',
    'end_time',
    'status',
    'distance_km',
    'duration_min',
    'start_location',
    'end_location',
    'start_lat',
    'start_lng',
    'end_lat',
    'end_lng',
    'start_battery_level',
    'end_battery_level',
    'start_range_km',
    'end_range_km',
    'max_speed_kmh',
    'ascent_m',
    'descent_m',
  ],
  positions: [
    'drive_id',
    'timestamp',
    'latitude',
    'longitude',
    'speed_kmh',
    'power_kw',
    'elevation_m',
    'battery_level',
    'outside_temp_c',
    'inside_temp_c',
    'tpms_pressure_fl',
    'tpms_pressure_fr',
    'tpms_pressure_rl',
    'tpms_pressure_rr',
  ],
  charging_sessions: [
    'vehicle_id',
    'start_time',
    'end_time',
    'status',
    'location',
    'latitude',
    'longitude',
    'start_battery_level',
    'end_battery_level',
    'charge_energy_added_kwh',
    'charge_energy_used_kwh',
    'max_charger_power_kw',
    'cost',
    'is_dc_fast_charge',
  ],
  charging_samples: [
    'charging_session_id',
    'timestamp',
    'battery_level',
    'charger_power_kw',
    'charge_energy_added_kwh',
  ],
  battery_samples: [
    'vehicle_id',
    'timestamp',
    'battery_level',
    'battery_range_km',
    'ideal_battery_range_km',
  ],
  software_updates: ['vehicle_id', 'version', 'status', 'start_time', 'end_time'],
}
class DatabaseError extends Error {
  override readonly name = 'DatabaseError'
}
const num = (v: SqlValue | undefined, n: string): number => {
  if (typeof v !== 'number') throw new DatabaseError(`Invalid ${n}.`)
  return v
}
const nullableNum = (v: SqlValue | undefined, n: string): number | null =>
  v === null ? null : num(v, n)
const str = (v: SqlValue | undefined, n: string): string => {
  if (typeof v !== 'string') throw new DatabaseError(`Invalid ${n}.`)
  return v
}
const one = (db: Database, sql: string): Readonly<Record<string, SqlValue>> => {
  const s = db.prepare(sql)
  try {
    if (!s.step()) throw new DatabaseError('No vehicle was found.')
    return s.getAsObject()
  } finally {
    s.free()
  }
}
const rows = (db: Database, sql: string): readonly Readonly<Record<string, SqlValue>>[] => {
  const s = db.prepare(sql)
  const output: Readonly<Record<string, SqlValue>>[] = []
  try {
    while (s.step()) output.push(s.getAsObject())
    return output
  } finally {
    s.free()
  }
}
const validate = (db: Database): void => {
  const result = db.exec("SELECT name FROM sqlite_schema WHERE type='table'")
  const names = new Set(
    (result[0]?.values ?? []).flatMap((row) => (typeof row[0] === 'string' ? [row[0]] : [])),
  )
  const missing = required.filter((name) => !names.has(name))
  if (missing.length)
    throw new DatabaseError(
      `Not a compatible teslalog database. Missing tables: ${missing.join(', ')}.`,
    )
  const missingColumns = Object.entries(requiredColumns).flatMap(([table, columns]) => {
    const info = db.exec(`PRAGMA table_info(${table})`)[0]
    const available = new Set(
      (info?.values ?? []).flatMap((row) => (typeof row[1] === 'string' ? [row[1]] : [])),
    )
    return columns.filter((column) => !available.has(column)).map((column) => `${table}.${column}`)
  })
  if (missingColumns.length)
    throw new DatabaseError(
      `This teslalog database uses an older or incompatible schema. Missing columns: ${missingColumns.join(', ')}.`,
    )
}
const overview = (db: Database) => {
  const r = one(
    db,
    `SELECT COALESCE(v.display_name,'Tesla') display_name,COALESCE(v.marketing_name,v.model,'') model,COALESCE(v.firmware_version,'') firmware,COALESCE((SELECT state FROM states WHERE vehicle_id=v.id ORDER BY id DESC LIMIT 1),'unknown') state,(SELECT battery_level FROM battery_samples WHERE vehicle_id=v.id ORDER BY timestamp DESC LIMIT 1) battery,(SELECT battery_range_km FROM battery_samples WHERE vehicle_id=v.id ORDER BY timestamp DESC LIMIT 1) range_km,(SELECT end_odometer_km FROM drives WHERE vehicle_id=v.id AND status='closed' ORDER BY end_time DESC LIMIT 1) odometer,(SELECT COUNT(*) FROM drives WHERE vehicle_id=v.id AND status='closed') drives,COALESCE((SELECT SUM(distance_km) FROM drives WHERE vehicle_id=v.id AND status='closed'),0) distance,(SELECT COUNT(*) FROM charging_sessions WHERE vehicle_id=v.id AND status='closed') charges,COALESCE((SELECT SUM(charge_energy_added_kwh) FROM charging_sessions WHERE vehicle_id=v.id AND status='closed'),0) energy FROM vehicles v ORDER BY v.id LIMIT 1`,
  )
  return vehicleSchema.parse({
    displayName: str(r.display_name, 'display name'),
    model: str(r.model, 'model'),
    firmware: str(r.firmware, 'firmware'),
    state: str(r.state, 'state'),
    battery: nullableNum(r.battery, 'battery'),
    rangeKm: nullableNum(r.range_km, 'range'),
    odometerKm: nullableNum(r.odometer, 'odometer'),
    drives: num(r.drives, 'drives'),
    distanceKm: num(r.distance, 'distance'),
    charges: num(r.charges, 'charges'),
    energyKwh: num(r.energy, 'energy'),
  })
}
const metric = (db: Database, label: string, sql: string, unit: string): Metric => ({
  label,
  value: num(one(db, sql).value, label),
  unit,
})
const drives = (db: Database) =>
  rows(
    db,
    `SELECT d.id, d.start_time time, COALESCE(d.start_location, printf('%.4f, %.4f', d.start_lat, d.start_lng)) "from", COALESCE(d.end_location, printf('%.4f, %.4f', d.end_lat, d.end_lng)) "to", ROUND(d.distance_km,1) distance_km, ROUND(d.duration_min,0) duration_min, d.start_battery_level start_battery, d.end_battery_level end_battery, ROUND(d.max_speed_kmh,0) max_speed_kmh, ROUND(d.ascent_m,0) ascent_m, ROUND(d.descent_m,0) descent_m, d.outside_temp_avg_c, d.distance_km/NULLIF(d.duration_min/60.0,0) average_speed_kmh, (d.start_range_km-d.end_range_km)*v.efficiency_wh_km energy_kwh, d.start_range_km-d.end_range_km range_diff_km, v.efficiency_wh_km car_efficiency
     FROM drives d JOIN vehicles v ON v.id=d.vehicle_id WHERE d.status='closed' ORDER BY d.start_time DESC LIMIT 500`,
  ).map((r) =>
    driveRowSchema.parse({
      id: num(r.id, 'drive id'),
      time: str(r.time, 'drive time'),
      from: str(r.from, 'origin'),
      to: str(r.to, 'destination'),
      distanceKm: nullableNum(r.distance_km, 'distance'),
      durationMin: nullableNum(r.duration_min, 'duration'),
      startBattery: nullableNum(r.start_battery, 'start battery'),
      endBattery: nullableNum(r.end_battery, 'end battery'),
      maxSpeedKmh: nullableNum(r.max_speed_kmh, 'speed'),
      ascentM: nullableNum(r.ascent_m, 'ascent'),
      descentM: nullableNum(r.descent_m, 'descent'),
      outsideTempC: nullableNum(r.outside_temp_avg_c, 'outside temperature'),
      averageSpeedKmh: nullableNum(r.average_speed_kmh, 'average speed'),
      energyKwh: nullableNum(r.energy_kwh, 'energy'),
      rangeDiffKm: nullableNum(r.range_diff_km, 'range difference'),
      carEfficiencyKwhKm: nullableNum(r.car_efficiency, 'car efficiency'),
    }),
  )
const charges = (db: Database) =>
  rows(
    db,
    `SELECT id, start_time time, COALESCE(location, printf('%.4f, %.4f', latitude, longitude)) location, CASE WHEN is_dc_fast_charge=1 THEN 'DC' ELSE 'AC' END type, start_battery_level start_battery, end_battery_level end_battery, ROUND(charge_energy_added_kwh,2) energy_added, ROUND(charge_energy_used_kwh,2) energy_used, ROUND(max_charger_power_kw,1) max_power, ROUND(cost,2) cost, ROUND((julianday(end_time)-julianday(start_time))*24*60,0) duration, ROUND(cost/NULLIF(charge_energy_used_kwh,0),2) cost_per_kwh, ROUND(charge_energy_added_kwh*100.0/NULLIF(charge_energy_used_kwh,0),0) efficiency, outside_temp_avg_c FROM charging_sessions WHERE status='closed' ORDER BY start_time DESC LIMIT 500`,
  ).map((r) =>
    chargeRowSchema.parse({
      id: num(r.id, 'charge id'),
      time: str(r.time, 'charge time'),
      location: str(r.location, 'charge location'),
      type: r.type === 'DC' ? 'DC' : 'AC',
      startBattery: nullableNum(r.start_battery, 'start battery'),
      endBattery: nullableNum(r.end_battery, 'end battery'),
      energyAddedKwh: nullableNum(r.energy_added, 'energy added'),
      energyUsedKwh: nullableNum(r.energy_used, 'energy used'),
      maxPowerKw: nullableNum(r.max_power, 'max power'),
      cost: nullableNum(r.cost, 'cost'),
      durationMin: nullableNum(r.duration, 'duration'),
      costPerKwh: nullableNum(r.cost_per_kwh, 'cost per kWh'),
      efficiencyPercent: nullableNum(r.efficiency, 'efficiency'),
      outsideTempC: nullableNum(r.outside_temp_avg_c, 'outside temperature'),
    }),
  )
const destinations = (db: Database): readonly Destination[] =>
  rows(
    db,
    `SELECT end_location destination, COUNT(*) visits FROM drives WHERE status='closed' AND end_location IS NOT NULL AND end_location!='' GROUP BY end_location ORDER BY COUNT(*) DESC LIMIT 10`,
  ).map((r) => ({ name: str(r.destination, 'destination'), visits: num(r.visits, 'visits') }))
export type QueryVariables = Readonly<{
  driveId?: number
  chargingSessionId?: number
  lengthUnit?: 'km' | 'mi'
  temperatureUnit?: 'C' | 'F'
  minimumIdleHours?: number
  timeRange?: TimeRange
}>
const rangeExpression = (range: TimeRange): string => {
  switch (range) {
    case '24h':
      return "datetime('now', '-24 hours')"
    case '7d':
      return "datetime('now', '-7 days')"
    case '30d':
      return "datetime('now', '-30 days')"
    case '90d':
      return "datetime('now', '-90 days')"
    case '1y':
      return "datetime('now', '-1 year')"
    case 'all':
      return "datetime('0000-01-01')"
  }
}

const interpolate = (sql: string, variables: QueryVariables): string => {
  const interpolated = sql
    .replaceAll('$drive_id', String(Math.trunc(variables.driveId ?? 0)))
    .replaceAll('$charging_session_id', String(Math.trunc(variables.chargingSessionId ?? 0)))
    .replaceAll('$min_idle_hours', String(variables.minimumIdleHours ?? 1))
    .replaceAll('$length_unit', variables.lengthUnit ?? 'km')
    .replaceAll('$temp_unit', variables.temperatureUnit ?? 'C')
  return variables.timeRange
    ? interpolated.replace(
        /datetime\('now',\s*'-\d+\s+(?:hours?|days?|months?|years?)'\)/giu,
        rangeExpression(variables.timeRange),
      )
    : interpolated
}

export const executeQueries = async (
  bytes: Uint8Array,
  queries: readonly string[],
  variables: QueryVariables = {},
): Promise<readonly QueryResult[]> => {
  const SQL = await initSqlJs({ locateFile: () => wasmUrl })
  const database = new SQL.Database(bytes)
  try {
    return queries.map((sql) => {
      const result = database.exec(interpolate(sql, variables))[0]
      if (!result) return { columns: [], rows: [] }
      return {
        columns: result.columns,
        rows: result.values.map((row) => row.map((value) => queryValueSchema.parse(value))),
      }
    })
  } finally {
    database.close()
  }
}

export const executeQuery = async (
  bytes: Uint8Array,
  sql: string,
  variables: QueryVariables = {},
): Promise<QueryResult> =>
  (await executeQueries(bytes, [sql], variables))[0] ?? { columns: [], rows: [] }

export const openDatabaseBytes = async (
  fileName: string,
  fileSize: number,
  databaseBytes: Uint8Array,
): Promise<LoadedDatabase> => {
  const SQL = await initSqlJs({ locateFile: () => wasmUrl })
  const db = new SQL.Database(databaseBytes)
  try {
    validate(db)
    return {
      fileName,
      fileSize,
      databaseBytes,
      vehicle: overview(db),
      drives: drives(db),
      driveMetrics: [
        metric(
          db,
          'Total drives (90d)',
          `SELECT COUNT(*) value FROM drives WHERE status='closed' AND start_time>=datetime('now','-90 days')`,
          'drives',
        ),
        metric(
          db,
          'Total distance (90d)',
          `SELECT COALESCE(SUM(distance_km),0) value FROM drives WHERE status='closed' AND start_time>=datetime('now','-90 days')`,
          'km',
        ),
        metric(
          db,
          'Average distance',
          `SELECT COALESCE(AVG(distance_km),0) value FROM drives WHERE status='closed' AND start_time>=datetime('now','-90 days')`,
          'km',
        ),
        metric(
          db,
          'Max speed (90d)',
          `SELECT COALESCE(MAX(max_speed_kmh),0) value FROM drives WHERE status='closed' AND start_time>=datetime('now','-90 days')`,
          'km/h',
        ),
      ],
      lifetimeDriveMetrics: [
        metric(
          db,
          'Total drives',
          `SELECT COUNT(*) value FROM drives WHERE status='closed'`,
          'drives',
        ),
        metric(
          db,
          'Total distance',
          `SELECT COALESCE(SUM(distance_km),0) value FROM drives WHERE status='closed'`,
          'km',
        ),
        metric(
          db,
          'Median distance',
          `SELECT COALESCE((SELECT distance_km FROM drives WHERE status='closed' AND distance_km IS NOT NULL ORDER BY distance_km LIMIT 1 OFFSET (SELECT COUNT(*) FROM drives WHERE status='closed' AND distance_km IS NOT NULL)/2),0) value`,
          'km',
        ),
        metric(
          db,
          'Max speed ever',
          `SELECT COALESCE(MAX(max_speed_kmh),0) value FROM drives WHERE status='closed'`,
          'km/h',
        ),
      ],
      destinations: destinations(db),
      charges: charges(db),
      chargeMetrics: [
        metric(
          db,
          'Total charges (90d)',
          `SELECT COUNT(*) value FROM charging_sessions WHERE status='closed' AND start_time>=datetime('now','-90 days')`,
          'charges',
        ),
        metric(
          db,
          'Energy added (90d)',
          `SELECT COALESCE(SUM(charge_energy_added_kwh),0) value FROM charging_sessions WHERE status='closed' AND start_time>=datetime('now','-90 days')`,
          'kWh',
        ),
        metric(
          db,
          'Total cost (90d)',
          `SELECT COALESCE(SUM(cost),0) value FROM charging_sessions WHERE status='closed' AND start_time>=datetime('now','-90 days')`,
          'cost',
        ),
        metric(
          db,
          'Max charger power',
          `SELECT COALESCE(MAX(max_charger_power_kw),0) value FROM charging_sessions WHERE status='closed' AND start_time>=datetime('now','-90 days')`,
          'kW',
        ),
      ],
    }
  } catch (error: unknown) {
    throw new DatabaseError(error instanceof Error ? error.message : 'Could not read the database.')
  } finally {
    db.close()
  }
}

export const openDatabase = async (file: File): Promise<LoadedDatabase> =>
  openDatabaseBytes(file.name, file.size, new Uint8Array(await file.arrayBuffer()))
