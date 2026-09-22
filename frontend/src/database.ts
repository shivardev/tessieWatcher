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
  type QueryValue,
  type IncompleteRow,
  type SpeedBand,
} from './domain'
import type { PreferredRange, StatisticsPeriod, TimeRange } from './viewSettings'
import {
  getRemoteBackend,
  runRemoteQueries,
  runRemoteQuery,
  type RemoteConfig,
} from './remoteBackend'
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
export const num = (v: SqlValue | undefined, n: string): number => {
  if (typeof v === 'number') return v
  // The remote (Layerbase) backend returns numerics as strings, and larger
  // floats can arrive with a thousands separator or in exponent form that
  // the generic coercion leaves as text. In a spot that explicitly expects
  // a number, parse leniently (strip commas; Number() handles exponents).
  if (typeof v === 'string') {
    const parsed = Number(v.replace(/,/g, ''))
    if (Number.isFinite(parsed)) return parsed

    // Layerbase's SQLite adapter can serialize a numeric expression as an
    // ISO timestamp, treating the number as days since the Unix epoch. This
    // has been observed for MAX(max_charger_power_kw): 2 becomes
    // 1970-01-03T00:00:00Z. Decode that representation only where the caller
    // explicitly requires a number.
    if (/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$/.test(v)) {
      const timestamp = Date.parse(v)
      if (Number.isFinite(timestamp)) return timestamp / 86_400_000
    }
  }
  throw new DatabaseError(`Invalid ${n}.`)
}
const nullableNum = (v: SqlValue | undefined, n: string): number | null =>
  v === null ? null : num(v, n)
const str = (v: SqlValue | undefined, n: string): string => {
  if (typeof v !== 'string') throw new DatabaseError(`Invalid ${n}.`)
  return v
}
// A Runner executes one SQL statement and returns its result. The load
// helpers below are written against this so the same queries run either
// against an in-browser sql.js database (a downloaded file) or the remote
// Layerbase HTTP API (querying in the cloud, no download).
export type Runner = (sql: string) => Promise<QueryResult>

// toRecords turns a QueryResult (columns + row arrays) into keyed objects,
// the shape every mapping helper reads.
const toRecords = (result: QueryResult): Record<string, QueryValue>[] =>
  result.rows.map((row) =>
    Object.fromEntries(result.columns.map((c, i) => [c, row[i] ?? null])),
  )

// sqlJsRunner backs a Runner with a local sql.js database. Synchronous work
// wrapped in a resolved promise, so one code path serves both backends.
const sqlJsRunner = (db: Database): Runner => (sql) => {
  const result = db.exec(sql)[0]
  return Promise.resolve(
    result
      ? {
          columns: result.columns,
          rows: result.values.map((row) => row.map((v: SqlValue) => queryValueSchema.parse(v))),
        }
      : { columns: [], rows: [] },
  )
}

const one = async (run: Runner, sql: string): Promise<Record<string, QueryValue>> => {
  const result = await run(sql)
  if (result.error) throw new DatabaseError(result.error)
  const records = toRecords(result)
  const first = records[0]
  if (first === undefined) throw new DatabaseError('No vehicle was found.')
  return first
}
const rows = async (run: Runner, sql: string): Promise<readonly Record<string, QueryValue>[]> => {
  const result = await run(sql)
  if (result.error) throw new DatabaseError(result.error)
  return toRecords(result)
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
const overview = async (run: Runner) => {
  const r = await one(
    run,
    `SELECT COALESCE(NULLIF(v.display_name,''), 'Model ' || NULLIF(v.model,''), 'Vehicle') display_name,TRIM(COALESCE('Model ' || NULLIF(v.model,''),'') || ' ' || COALESCE(NULLIF(v.marketing_name,''),'')) model,COALESCE(v.firmware_version,'') firmware,COALESCE((SELECT state FROM states WHERE vehicle_id=v.id ORDER BY id DESC LIMIT 1),'unknown') state,(SELECT battery_level FROM battery_samples WHERE vehicle_id=v.id ORDER BY timestamp DESC LIMIT 1) battery,(SELECT battery_range_km FROM battery_samples WHERE vehicle_id=v.id ORDER BY timestamp DESC LIMIT 1) range_km,(SELECT end_odometer_km FROM drives WHERE vehicle_id=v.id AND status='closed' ORDER BY end_time DESC LIMIT 1) odometer,(SELECT COUNT(*) FROM drives WHERE vehicle_id=v.id AND status='closed') drives,COALESCE((SELECT SUM(distance_km) FROM drives WHERE vehicle_id=v.id AND status='closed'),0) distance,(SELECT COUNT(*) FROM charging_sessions WHERE vehicle_id=v.id AND status='closed') charges,COALESCE((SELECT SUM(charge_energy_added_kwh) FROM charging_sessions WHERE vehicle_id=v.id AND status='closed'),0) energy FROM vehicles v ORDER BY v.id LIMIT 1`,
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
const metric = async (run: Runner, label: string, sql: string, unit: string): Promise<Metric> => {
  const result = await rows(run, sql)
  const value = result[0]?.value
  return {
    label,
    value: value == null ? 0 : num(value, label),
    unit,
  }
}
// metrics runs a list of [label, sql, unit] specs in order. Sequential, not
// parallel, so the remote backend fires one HTTP request at a time and
// stays clear of the cloud rate limit.
const metrics = async (
  run: Runner,
  specs: readonly (readonly [string, string, string])[],
): Promise<Metric[]> => {
  const out: Metric[] = []
  for (const [label, sql, unit] of specs) out.push(await metric(run, label, sql, unit))
  return out
}
const drives = async (run: Runner) =>
  (await rows(
    run,
    `SELECT d.id, d.start_time time, COALESCE(d.start_location, printf('%.4f, %.4f', d.start_lat, d.start_lng)) "from", COALESCE(d.end_location, printf('%.4f, %.4f', d.end_lat, d.end_lng)) "to", ROUND(d.distance_km,1) distance_km, ROUND(d.duration_min,0) duration_min, d.start_battery_level start_battery, d.end_battery_level end_battery, ROUND(d.max_speed_kmh,0) max_speed_kmh, ROUND(d.max_power_kw,0) max_power_kw, ROUND(d.ascent_m,0) ascent_m, ROUND(d.descent_m,0) descent_m, d.outside_temp_avg_c, d.distance_km/NULLIF(d.duration_min/60.0,0) average_speed_kmh, (d.start_range_km-d.end_range_km)*v.efficiency_wh_km/1000.0 energy_kwh, d.start_range_km-d.end_range_km range_diff_km, v.efficiency_wh_km car_efficiency, rr.has_reduced_range
     FROM drives d JOIN vehicles v ON v.id=d.vehicle_id
     LEFT JOIN (
       -- TeslaMate's has_reduced_range, one pass over positions rather than
       -- a per-drive correlated subquery: 1 when >25% of a drive's samples
       -- show battery_level above usable_battery_level (cold-weather drop).
       SELECT drive_id,
              CASE WHEN SUM(CASE WHEN battery_level - usable_battery_level > 0 THEN 1 ELSE 0 END) * 1.0 / COUNT(*) > 0.25
                   THEN 1 ELSE 0 END has_reduced_range
       FROM positions
       WHERE drive_id IS NOT NULL AND battery_level IS NOT NULL AND usable_battery_level IS NOT NULL
       GROUP BY drive_id
     ) rr ON rr.drive_id = d.id
     WHERE d.status='closed' ORDER BY d.start_time DESC LIMIT 500`,
  )).map((r) =>
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
      maxPowerKw: nullableNum(r.max_power_kw, 'max power'),
      ascentM: nullableNum(r.ascent_m, 'ascent'),
      descentM: nullableNum(r.descent_m, 'descent'),
      outsideTempC: nullableNum(r.outside_temp_avg_c, 'outside temperature'),
      averageSpeedKmh: nullableNum(r.average_speed_kmh, 'average speed'),
      energyKwh: nullableNum(r.energy_kwh, 'energy'),
      rangeDiffKm: nullableNum(r.range_diff_km, 'range difference'),
      carEfficiencyKwhKm: nullableNum(r.car_efficiency, 'car efficiency'),
      hasReducedRange: nullableNum(r.has_reduced_range, 'reduced range'),
    }),
  )
const charges = async (run: Runner) =>
  (await rows(
    run,
    `WITH charge_data AS (
       SELECT *, CASE
         WHEN charge_energy_used_kwh IS NULL THEN charge_energy_added_kwh
         WHEN charge_energy_added_kwh IS NULL THEN charge_energy_used_kwh
         ELSE MAX(charge_energy_used_kwh, charge_energy_added_kwh)
       END AS displayed_energy_used
       FROM charging_sessions WHERE status='closed'
     )
     SELECT id, start_time time, COALESCE(location, printf('%.4f, %.4f', latitude, longitude)) location,
       CASE WHEN is_dc_fast_charge=1 THEN 'DC' ELSE 'AC' END type,
       start_battery_level start_battery, end_battery_level end_battery,
       charge_energy_added_kwh energy_added, displayed_energy_used energy_used,
       max_charger_power_kw max_power, cost,
       (julianday(end_time)-julianday(start_time))*24*60 duration,
       cost/NULLIF(displayed_energy_used,0) cost_per_kwh,
       charge_energy_added_kwh*100.0/NULLIF(displayed_energy_used,0) efficiency,
       outside_temp_avg_c,
       end_range_km-start_range_km rated_range_added_km,
       end_ideal_range_km-start_ideal_range_km ideal_range_added_km,
       NULL odometer_km
     FROM charge_data ORDER BY start_time DESC LIMIT 500`,
  )).map((r) =>
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
      ratedRangeAddedKm: nullableNum(r.rated_range_added_km, 'rated range added'),
      idealRangeAddedKm: nullableNum(r.ideal_range_added_km, 'ideal range added'),
      // TeslaMate links a charge to a position with an odometer reading.
      // teslalog's charging_sessions and charging_samples do not store it.
      odometerKm: nullableNum(r.odometer_km, 'charge odometer'),
    }),
  )
// incompleteDrives / incompleteCharges list rows that were opened and
// never closed - the car went offline mid-drive, or teslalog was stopped
// while charging. TeslaMate surfaces the same two tables (end_date IS
// NULL) because these rows silently distort every total that sums over
// them, and the only way to notice is to be shown them.
const incompleteDrives = async (run: Runner): Promise<readonly IncompleteRow[]> =>
  (await rows(
    run,
    `SELECT id, start_time, end_time, ROUND(distance_km,2) AS a, ROUND(duration_min,1) AS b,
            start_battery_level AS c, end_battery_level AS d
     FROM drives WHERE status != 'closed' ORDER BY start_time DESC LIMIT 100`,
  )).map((r) => ({
    id: num(r.id, 'drive id'),
    startTime: str(r.start_time, 'start time'),
    endTime: typeof r.end_time === 'string' ? r.end_time : null,
    values: [nullableNum(r.a, 'distance'), nullableNum(r.b, 'duration'), nullableNum(r.c, 'start battery'), nullableNum(r.d, 'end battery')],
  }))

const incompleteCharges = async (run: Runner): Promise<readonly IncompleteRow[]> =>
  (await rows(
    run,
    `SELECT id, start_time, end_time, ROUND(charge_energy_added_kwh,2) AS a, ROUND(charge_energy_used_kwh,2) AS b,
            start_battery_level AS c, end_battery_level AS d
     FROM charging_sessions WHERE status != 'closed' ORDER BY start_time DESC LIMIT 100`,
  )).map((r) => ({
    id: num(r.id, 'charge id'),
    startTime: str(r.start_time, 'start time'),
    endTime: typeof r.end_time === 'string' ? r.end_time : null,
    values: [nullableNum(r.a, 'energy added'), nullableNum(r.b, 'energy used'), nullableNum(r.c, 'start battery'), nullableNum(r.d, 'end battery')],
  }))

const destinations = async (run: Runner): Promise<readonly Destination[]> =>
  (await rows(
    run,
    `SELECT end_location destination, COUNT(*) visits FROM drives WHERE status='closed' AND end_location IS NOT NULL AND end_location!='' GROUP BY end_location ORDER BY COUNT(*) DESC LIMIT 10`,
  )).map((r) => ({ name: str(r.destination, 'destination'), visits: num(r.visits, 'visits') }))
// speedHistogram weights each 10 km/h band by the seconds the car spent
// in it, taken as the gap to the next position sample within the same
// drive. Counting samples instead would let a dense stretch of slow
// city driving dominate a sparsely sampled motorway run.
const speedHistogram = async (run: Runner): Promise<readonly SpeedBand[]> =>
  (await rows(
    run,
    `WITH d AS (
       SELECT ROUND(speed_kmh/10.0)*10 AS band,
              (julianday(LEAD(timestamp) OVER (PARTITION BY drive_id ORDER BY timestamp))
               - julianday(timestamp)) * 86400 AS seconds
       FROM positions WHERE drive_id IS NOT NULL AND speed_kmh IS NOT NULL
     )
     SELECT band, SUM(seconds) seconds FROM d
     WHERE band > 0 AND seconds IS NOT NULL AND seconds < 600
     GROUP BY band ORDER BY band`,
  )).map((r) => ({ speedKmh: num(r.band, 'speed band'), seconds: num(r.seconds, 'seconds') }))

export type QueryVariables = Readonly<{
  driveId?: number
  chargingSessionId?: number
  lengthUnit?: 'km' | 'mi'
  temperatureUnit?: 'C' | 'F'
  minimumIdleHours?: number
  timeRange?: TimeRange
  preferredRange?: PreferredRange
  minDistance?: number
  statisticsPeriod?: StatisticsPeriod
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

// queryVariables is the complete set of $names a catalog query may use.
// Declared as data rather than a chain of replaceAll calls so the
// catalog test can assert the dashboards use nothing that is not listed
// here - a query referencing an unknown $variable would otherwise reach
// SQLite verbatim and fail at runtime, on one dashboard, for one user.
//
// $preferred_range expands to the word "rated"/"ideal" for labels;
// $start_range/$end_range/$pos_range expand to whole column names.
// teslalog does not repeat "rated" in its column names the way TeslaMate
// does (start_range_km, not start_rated_range_km), so the column name is
// substituted rather than an infix.
export const queryVariables = {
  $drive_id: (v: QueryVariables) => String(Math.trunc(v.driveId ?? 0)),
  $charging_session_id: (v: QueryVariables) => String(Math.trunc(v.chargingSessionId ?? 0)),
  $min_idle_hours: (v: QueryVariables) => String(v.minimumIdleHours ?? 1),
  $min_distance: (v: QueryVariables) => String(v.minDistance ?? 0),
  $length_unit: (v: QueryVariables) => v.lengthUnit ?? 'km',
  $temp_unit: (v: QueryVariables) => v.temperatureUnit ?? 'C',
  $period: (v: QueryVariables) => v.statisticsPeriod ?? 'month',
  $preferred_range: (v: QueryVariables) => v.preferredRange ?? 'rated',
  $start_range: (v: QueryVariables) =>
    v.preferredRange === 'ideal' ? 'start_ideal_range_km' : 'start_range_km',
  $end_range: (v: QueryVariables) =>
    v.preferredRange === 'ideal' ? 'end_ideal_range_km' : 'end_range_km',
  $pos_range: (v: QueryVariables) => (v.preferredRange === 'ideal' ? 'ideal_range_km' : 'range_km'),
} as const

// Longest name first, so $preferred_range is never partly eaten by a
// shorter name that happens to be a prefix of it.
const substitutionOrder = Object.keys(queryVariables).toSorted((a, b) => b.length - a.length)

// interpolateLabel expands the same $variables in a panel title or
// column label. Titles carry units the query cannot ("Ø Consumption
// (Wh/$length_unit)"), and leaving them raw does more than look wrong:
// the viewer's unit conversion reads the title as a fallback hint, so an
// uninterpolated "$preferred_range" contains the word "range" and gets
// the value converted as though it were a distance.
export const interpolateLabel = (label: string, variables: QueryVariables): string =>
  substitutionOrder.reduce(
    (text, name) =>
      text.replaceAll(name, queryVariables[name as keyof typeof queryVariables](variables)),
    label,
  )

const interpolate = (sql: string, variables: QueryVariables): string => {
  const interpolated = substitutionOrder.reduce(
    (query, name) =>
      query.replaceAll(name, queryVariables[name as keyof typeof queryVariables](variables)),
    sql,
  )
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
  // When a cloud backend is connected, the same interpolated SQL runs
  // against Layerbase's HTTP query API instead of a local sql.js copy -
  // no downloaded database, bytes is ignored. The dashboards are unchanged;
  // only where the query executes moves.
  const remote = getRemoteBackend()
  if (remote !== null)
    return runRemoteQueries(
      remote,
      queries.map((sql) => interpolate(sql, variables)),
    )

  const SQL = await initSqlJs({ locateFile: () => wasmUrl })
  const database = new SQL.Database(bytes)
  try {
    // A query that fails returns an empty result carrying the reason,
    // rather than throwing. One panel referencing a column an older
    // teslalog database does not have used to blank the entire
    // dashboard, because every panel's query runs in one batch - the
    // Locations page went dark for a database written before the address
    // columns existed, when eight of its nine panels were fine.
    return queries.map((sql) => {
      try {
        const result = database.exec(interpolate(sql, variables))[0]
        if (!result) return { columns: [], rows: [] }
        return {
          columns: result.columns,
          rows: result.values.map((row) => row.map((value) => queryValueSchema.parse(value))),
        }
      } catch (reason: unknown) {
        return {
          columns: [],
          rows: [],
          error: reason instanceof Error ? reason.message : 'Query failed.',
        }
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

// buildLoaded assembles the whole LoadedDatabase from a Runner, so the
// exact same queries drive the local (sql.js) and remote (Layerbase HTTP)
// paths. Awaited in order - fine for sql.js, and keeps the remote backend
// to one request at a time (rate-limit safe).
const buildLoaded = async (
  run: Runner,
  meta: Pick<LoadedDatabase, 'fileName' | 'fileSize' | 'databaseBytes'>,
): Promise<LoadedDatabase> => ({
  ...meta,
  vehicle: await overview(run),
  drives: await drives(run),
  driveMetrics: await metrics(run, [
    ['Total drives (90d)', `SELECT COUNT(*) value FROM drives WHERE status='closed' AND start_time>=datetime('now','-90 days')`, 'drives'],
    ['Total distance (90d)', `SELECT SUM(distance_km) value FROM drives WHERE status='closed' AND start_time>=datetime('now','-90 days')`, 'km'],
    ['Average distance', `SELECT AVG(distance_km) value FROM drives WHERE status='closed' AND start_time>=datetime('now','-90 days')`, 'km'],
    ['Max speed (90d)', `SELECT MAX(max_speed_kmh) value FROM drives WHERE status='closed' AND start_time>=datetime('now','-90 days')`, 'km/h'],
  ]),
  lifetimeDriveMetrics: await metrics(run, [
    ['Total drives', `SELECT COUNT(*) value FROM drives WHERE status='closed'`, 'drives'],
    ['Total distance', `SELECT SUM(distance_km) value FROM drives WHERE status='closed'`, 'km'],
    ['Median distance', `SELECT (SELECT distance_km FROM drives WHERE status='closed' AND distance_km IS NOT NULL ORDER BY distance_km LIMIT 1 OFFSET (SELECT COUNT(*) FROM drives WHERE status='closed' AND distance_km IS NOT NULL)/2) value`, 'km'],
    ['Max speed ever', `SELECT MAX(max_speed_kmh) value FROM drives WHERE status='closed'`, 'km/h'],
    // Net energy: range lost while driving, priced at the car's Wh/km; /1000
    // because efficiency_wh_km is Wh per km and the card reads kWh.
    ['Total energy consumed (net)', `SELECT SUM((d.start_range_km-d.end_range_km)*v.efficiency_wh_km)/1000.0 value FROM drives d JOIN vehicles v ON v.id=d.vehicle_id WHERE d.status='closed'`, 'kWh'],
    // Per-day averages divide by elapsed calendar days, not days-with-a-drive.
    ['Ø distance per day', `SELECT SUM(distance_km)/MAX(1.0,julianday(MAX(end_time))-julianday(MIN(start_time))) value FROM drives WHERE status='closed'`, 'km'],
    ['Ø energy per day', `SELECT SUM((d.start_range_km-d.end_range_km)*v.efficiency_wh_km)/1000.0/MAX(1.0,julianday(MAX(d.end_time))-julianday(MIN(d.start_time))) value FROM drives d JOIN vehicles v ON v.id=d.vehicle_id WHERE d.status='closed'`, 'kWh'],
    // Extrapolations use odometer delta, so driving missed while teslalog was
    // down still counts - the odometer is the car's own record.
    ['Extrapolated monthly mileage', `SELECT (MAX(end_odometer_km)-MIN(start_odometer_km))/MAX(1.0,julianday(MAX(end_time))-julianday(MIN(start_time)))*(365.0/12) value FROM drives WHERE status='closed'`, 'km'],
    ['Extrapolated annual mileage', `SELECT (MAX(end_odometer_km)-MIN(start_odometer_km))/MAX(1.0,julianday(MAX(end_time))-julianday(MIN(start_time)))*365.0 value FROM drives WHERE status='closed'`, 'km'],
  ]),
  speedHistogram: await speedHistogram(run),
  destinations: await destinations(run),
  charges: await charges(run),
  chargeMetrics: await metrics(run, [
    ['Total charges (90d)', `SELECT COUNT(*) value FROM charging_sessions WHERE status='closed' AND start_time>=datetime('now','-90 days')`, 'charges'],
    ['Energy added (90d)', `SELECT SUM(charge_energy_added_kwh) value FROM charging_sessions WHERE status='closed' AND start_time>=datetime('now','-90 days')`, 'kWh'],
    ['Total cost (90d)', `SELECT SUM(cost) value FROM charging_sessions WHERE status='closed' AND start_time>=datetime('now','-90 days')`, 'cost'],
    ['Max charger power', `SELECT max_charger_power_kw value FROM charging_sessions WHERE status='closed' AND start_time>=datetime('now','-90 days') AND max_charger_power_kw IS NOT NULL ORDER BY CAST(max_charger_power_kw AS REAL) DESC LIMIT 1`, 'kW'],
  ]),
  incompleteDrives: await incompleteDrives(run),
  incompleteCharges: await incompleteCharges(run),
})

export const openDatabaseBytes = async (
  fileName: string,
  fileSize: number,
  databaseBytes: Uint8Array,
): Promise<LoadedDatabase> => {
  const SQL = await initSqlJs({ locateFile: () => wasmUrl })
  const db = new SQL.Database(databaseBytes)
  try {
    validate(db)
    return await buildLoaded(sqlJsRunner(db), { fileName, fileSize, databaseBytes })
  } catch (error: unknown) {
    throw new DatabaseError(error instanceof Error ? error.message : 'Could not read the database.')
  } finally {
    db.close()
  }
}

// openDatabaseRemote builds the same LoadedDatabase by querying a Layerbase
// cloud database over HTTP - no file downloaded. databaseBytes is empty
// because nothing local is held; the dashboards read remotely too via the
// active remote backend (see executeQueries), which the caller sets.
export const openDatabaseRemote = async (config: RemoteConfig): Promise<LoadedDatabase> => {
  try {
    return await buildLoaded((sql) => runRemoteQuery(config, sql), {
      fileName: 'cloud',
      fileSize: 0,
      databaseBytes: new Uint8Array(0),
    })
  } catch (error: unknown) {
    throw new DatabaseError(
      error instanceof Error ? error.message : 'Could not read the cloud database.',
    )
  }
}

export const openDatabase = async (file: File): Promise<LoadedDatabase> =>
  openDatabaseBytes(file.name, file.size, new Uint8Array(await file.arrayBuffer()))
