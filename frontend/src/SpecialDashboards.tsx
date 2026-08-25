import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import type { LatLngBoundsExpression, LatLngExpression } from 'leaflet'
import { CircleMarker, MapContainer, Polyline, TileLayer, Tooltip as LeafletTooltip, useMap } from 'react-leaflet'
import 'leaflet/dist/leaflet.css'
import {
  CartesianGrid,
  Line,
  LineChart,
  Pie,
  PieChart,
  Cell,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
  type TooltipContentProps,
} from 'recharts'
import { executeQueries, type QueryVariables } from './database'
import { dashboardCatalog } from './catalog'
import { epochMs, nearestTimeSync } from './chartSync'
import { StateTimeline } from './StateTimeline'
import type { QueryResult, QueryValue } from './domain'
import { distance, speed, timeRangeSql, timestampDate, type LengthUnit, type ViewSettings } from './viewSettings'

type Point = Readonly<{ latitude: number; longitude: number; timestamp?: string; speedKmh?: number }>

// speedColor maps a speed to the route colour: blue when slow, green at
// an easy cruise, red when fast. The scale is anchored to the drive's own
// fast end (vmax) rather than a fixed number, so a city errand and a
// highway run each use the full range and read clearly. Hue runs 210°
// (blue) down through 120° (green) to 0° (red); green therefore lands
// around 43% of vmax, which is normal cruising, matching "green eco".
const speedColor = (speedKmh: number, vmaxKmh: number): string => {
  const t = vmaxKmh <= 0 ? 0 : Math.max(0, Math.min(1, speedKmh / vmaxKmh))
  const hue = (1 - t) * 210
  return `hsl(${hue.toFixed(0)}, 85%, 48%)`
}
const text = (value: QueryValue | undefined): string =>
  value === null || value === undefined
    ? '—'
    : value instanceof Uint8Array
      ? 'binary'
      : String(value)
const number = (value: QueryValue | undefined): number | null =>
  typeof value === 'number' ? value : null

function useResults(
  bytes: Uint8Array,
  queries: readonly string[],
  variables: QueryVariables = {},
): Readonly<{ results: readonly QueryResult[]; error: string | null; loading: boolean }> {
  const [results, setResults] = useState<readonly QueryResult[]>([])
  const [error, setError] = useState<string | null>(null)
  // Starts true: the first render happens before any query has run, and
  // reporting that as "loaded with no data" is what made a working
  // dashboard look broken - every value an em-dash, every chart claiming
  // no telemetry, for the several seconds sql.js needs on a large
  // database.
  const [loading, setLoading] = useState(true)

  // Memoised on the serialised content rather than a hand-listed set of
  // fields. The list version had silently fallen behind: preferredRange,
  // minDistance and statisticsPeriod were passed in by callers and
  // dropped here, so the Range type and Min drive controls did nothing on
  // any of these dashboards while appearing to work.
  const variablesKey = JSON.stringify(variables)
  const stableVariables = useMemo<QueryVariables>(
    () => JSON.parse(variablesKey) as QueryVariables,
    [variablesKey],
  )

  useEffect(() => {
    let active = true
    setLoading(true)
    executeQueries(bytes, queries, stableVariables)
      .then((value) => {
        if (active) {
          setResults(value)
          setError(null)
        }
      })
      .catch((reason: unknown) => {
        if (active) setError(reason instanceof Error ? reason.message : 'Query failed.')
      })
      .finally(() => {
        if (active) setLoading(false)
      })
    return () => {
      active = false
    }
  }, [bytes, queries, stableVariables])
  return { results, error, loading }
}

// Shown while a dashboard's queries are in flight. Reading a database of
// a million positions in sql.js takes seconds, and without this the page
// renders its finished layout full of em-dashes and "no data" notices -
// which reads as broken rather than busy.
function DashboardLoading({ title }: Readonly<{ title: string }>) {
  return (
    <main>
      <Heading title={title} note="Loading" />
      <p className="dashboard-loading" role="status" aria-live="polite">
        <span className="spinner" aria-hidden="true" />
        Reading the database…
      </p>
    </main>
  )
}

function Heading({ title, note }: Readonly<{ title: string; note: string }>) {
  return (
    <header className="page-head">
      <div>
        <span className="eyebrow">{note}</span>
        <h1>{title}</h1>
      </div>
    </header>
  )
}

export function DataTable({ result }: Readonly<{ result: QueryResult | undefined }>) {
  if (!result) return <p className="no-data">Loading…</p>
  return (
    <div className="table-panel">
      <table>
        <thead>
          <tr>
            {result.columns.map((column) => (
              <th key={column}>{column}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {result.rows.map((row, rowIndex) => (
            <tr key={rowIndex}>
              {row.map((value, columnIndex) => (
                <td key={`${rowIndex}-${columnIndex}`}>{text(value)}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      {result.rows.length === 0 && <p className="no-data">No matching data.</p>}
    </div>
  )
}

export function LocalPlot({ points }: Readonly<{ points: readonly Point[] }>) {
  const canvas = useRef<HTMLCanvasElement>(null)
  useEffect(() => {
    const element = canvas.current
    const context = element?.getContext('2d')
    if (!element || !context || points.length === 0) return
    const ratio = window.devicePixelRatio || 1
    const width = element.clientWidth
    const height = element.clientHeight
    element.width = width * ratio
    element.height = height * ratio
    context.scale(ratio, ratio)
    let minLat = Number.POSITIVE_INFINITY
    let maxLat = Number.NEGATIVE_INFINITY
    let minLng = Number.POSITIVE_INFINITY
    let maxLng = Number.NEGATIVE_INFINITY
    for (const point of points) {
      minLat = Math.min(minLat, point.latitude)
      maxLat = Math.max(maxLat, point.latitude)
      minLng = Math.min(minLng, point.longitude)
      maxLng = Math.max(maxLng, point.longitude)
    }
    const x = (longitude: number): number =>
      24 + ((longitude - minLng) / Math.max(maxLng - minLng, 0.00001)) * (width - 48)
    const y = (latitude: number): number =>
      height - 24 - ((latitude - minLat) / Math.max(maxLat - minLat, 0.00001)) * (height - 48)
    context.clearRect(0, 0, width, height)
    context.strokeStyle = '#26322f'
    context.lineWidth = 1
    for (let line = 1; line < 5; line += 1) {
      context.beginPath()
      context.moveTo(0, (height * line) / 5)
      context.lineTo(width, (height * line) / 5)
      context.stroke()
    }
    context.strokeStyle = '#c9ff43'
    context.lineWidth = 3
    context.lineJoin = 'round'
    context.beginPath()
    const sampleEvery = Math.max(1, Math.ceil(points.length / 10_000))
    const plottedPoints = points.filter(
      (_, index) => index % sampleEvery === 0 || index === points.length - 1,
    )
    plottedPoints.forEach((point, index) =>
      index === 0
        ? context.moveTo(x(point.longitude), y(point.latitude))
        : context.lineTo(x(point.longitude), y(point.latitude)),
    )
    context.stroke()
  }, [points])
  return (
    <canvas
      className="local-plot"
      ref={canvas}
      aria-label="Local route plot without external map tiles"
    />
  )
}

function FitRoute({ points }: Readonly<{ points: readonly LatLngExpression[] }>) {
  const map = useMap()
  useEffect(() => {
    if (points.length === 1) {
      map.setView(points[0] ?? [0, 0], 15)
      return
    }
    if (points.length < 2) return
    map.fitBounds(points as LatLngBoundsExpression, { padding: [24, 24] })
  }, [map, points])
  return null
}

// speedSegments turns the route into consecutive coloured segments when
// the points carry speed. Segments are capped at ~600 so a drive with
// thousands of positions does not spawn thousands of Leaflet layers; the
// route is thinned by an even stride, and each segment is coloured by the
// faster of its two endpoints so a brief slow sample does not wash out a
// fast stretch. Returns null when there is no usable speed, and the map
// falls back to a single line.
type Segment = Readonly<{ positions: readonly LatLngExpression[]; color: string }>
const speedSegments = (points: readonly Point[]): { segments: readonly Segment[]; vmaxKmh: number } | null => {
  const speeds = points.flatMap((p) => (typeof p.speedKmh === 'number' ? [p.speedKmh] : []))
  if (speeds.length < 2) return null
  // 95th percentile as the "fast" anchor, so one GPS speed spike does not
  // push the whole scale into blue.
  const sorted = [...speeds].toSorted((a, b) => a - b)
  const vmaxKmh = sorted[Math.floor(sorted.length * 0.95)] ?? sorted[sorted.length - 1] ?? 0
  if (vmaxKmh <= 0) return null

  const stride = Math.max(1, Math.ceil(points.length / 600))
  const thinned = points.filter((_, index) => index % stride === 0 || index === points.length - 1)
  const segments: Segment[] = []
  for (let i = 0; i < thinned.length - 1; i++) {
    const a = thinned[i]!
    const b = thinned[i + 1]!
    const speed = Math.max(a.speedKmh ?? 0, b.speedKmh ?? 0)
    segments.push({
      positions: [
        [a.latitude, a.longitude],
        [b.latitude, b.longitude],
      ],
      color: speedColor(speed, vmaxKmh),
    })
  }
  return { segments, vmaxKmh }
}

function RouteMap({
  points,
  activePoint,
  lengthUnit,
}: Readonly<{ points: readonly Point[]; activePoint?: Point | undefined; lengthUnit?: LengthUnit }>) {
  const positions = useMemo<readonly LatLngExpression[]>(
    () => points.map((point) => [point.latitude, point.longitude]),
    [points],
  )
  const speedRoute = useMemo(() => speedSegments(points), [points])
  if (positions.length === 0) return <p className="no-data">No route coordinates recorded.</p>

  const unit = lengthUnit ?? 'km'
  const fast = speedRoute ? speed(speedRoute.vmaxKmh, unit) : 0
  return (
    <div className="route-map-wrap">
      <MapContainer className="route-map" center={positions[0] ?? [0, 0]} zoom={13} scrollWheelZoom>
        <TileLayer
          attribution='&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors'
          url="https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png"
        />
        {speedRoute ? (
          speedRoute.segments.map((segment, index) => (
            <Polyline
              key={index}
              positions={[...segment.positions]}
              pathOptions={{ color: segment.color, weight: 4, opacity: 0.95 }}
            />
          ))
        ) : (
          <Polyline positions={[...positions]} pathOptions={{ color: '#2455d6', weight: 4 }} />
        )}
        {positions.length === 1 && <CircleMarker center={positions[0] ?? [0, 0]} radius={8} pathOptions={{ color: '#c9ff43', fillColor: '#c9ff43', fillOpacity: 0.85 }} />}
        {activePoint && <CircleMarker center={[activePoint.latitude, activePoint.longitude]} radius={7} pathOptions={{ color: '#ffffff', weight: 2, fillColor: '#c9ff43', fillOpacity: 1 }} />}
        <FitRoute points={positions} />
      </MapContainer>
      {speedRoute && (
        <div className="route-speed-legend" aria-hidden="true">
          <span>slow</span>
          <i className="route-speed-gradient" />
          <span>{`${Math.round(fast)} ${unit}/h`}</span>
        </div>
      )}
    </div>
  )
}

function ChargingSitesMap({ result }: Readonly<{ result: QueryResult | undefined }>) {
  const sites = (result?.rows ?? []).flatMap((row) => {
    const latitude = number(row[1])
    const longitude = number(row[2])
    const energy = number(row[3])
    return latitude === null || longitude === null || energy === null
      ? []
      : [{ name: text(row[0]), latitude, longitude, energy, sessions: number(row[4]) ?? 0 }]
  })
  if (sites.length === 0) return <p className="no-data">No charging coordinates recorded.</p>
  const positions = sites.map((site): LatLngExpression => [site.latitude, site.longitude])
  return (
    <MapContainer className="route-map" center={positions[0] ?? [0,0]} zoom={9} scrollWheelZoom>
      <TileLayer attribution='&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors' url="https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png" />
      {sites.map((site) => <CircleMarker key={`${site.latitude}-${site.longitude}`} center={[site.latitude,site.longitude]} radius={Math.max(6,Math.min(22,Math.sqrt(site.energy)*2))} pathOptions={{color:'#fade2a',fillColor:'#ff9830',fillOpacity:.7}}><LeafletTooltip>{site.name}: {site.energy.toFixed(1)} kWh · {site.sessions} sessions</LeafletTooltip></CircleMarker>)}
      <FitRoute points={positions}/>
    </MapContainer>
  )
}

const telemetryColors = ['#8ab4ff', '#ff8a00', '#b15cff', '#76d275', '#ffd54f'] as const
const chartTime = (value: string | number): string => {
  const date = timestampDate(value)
  return Number.isNaN(date.getTime())
    ? String(value)
    : date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}
const tooltipTime = (value: ReactNode): ReactNode =>
  typeof value === 'string' || typeof value === 'number' ? chartTime(value) : value

const booleanSeries = (name: string): boolean => /battery heater|climate/iu.test(name)
const telemetryValue = (value: number, name: string): string => {
  if (booleanSeries(name)) return value >= 0.5 ? 'On' : 'Off'
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 }).format(value)
}
const scaleForSeries = (name: string): string => {
  if (/range/iu.test(name)) return 'range'
  if (/soc|%/iu.test(name)) return 'percent'
  if (booleanSeries(name)) return 'boolean'
  if (/fan/iu.test(name)) return 'fan'
  if (/temperature|outside|inside|driver|passenger|°/iu.test(name)) return 'temperature'
  if (/pressure|bar|psi/iu.test(name)) return 'pressure'
  if (/elevation/iu.test(name)) return 'elevation'
  return 'motion'
}

function CompactTelemetryTooltip({
  active,
  label,
  payload,
  labelAsTime = true,
  xLabel = 'Time',
}: TooltipContentProps & Readonly<{ labelAsTime?: boolean; xLabel?: string }>) {
  const entries = (payload ?? []).flatMap((entry) =>
    typeof entry.value === 'number' && typeof entry.name === 'string'
      ? [
          {
            color: entry.color,
            key: entry.dataKey?.toString() ?? entry.name,
            name: entry.name,
            value: entry.value,
          },
        ]
      : [],
  )
  if (!active || entries.length === 0) return null
  return (
    <div className="compact-tooltip">
      <strong>
        {labelAsTime
          ? tooltipTime(label)
          : `${xLabel}: ${typeof label === 'number' ? telemetryValue(label, xLabel) : String(label ?? '—')}`}
      </strong>
      {entries.map((entry) => (
        <div key={entry.key}>
          <i style={{ background: entry.color }} />
          <span>{entry.name}</span>
          <b>{telemetryValue(entry.value, entry.name)}</b>
        </div>
      ))}
    </div>
  )
}

function TelemetryChart({
  result,
  title,
  emptyMessage = 'No telemetry recorded for this drive.',
  onHoverTime,
}: Readonly<{
  result: QueryResult | undefined
  title: string
  emptyMessage?: string
  onHoverTime?: (timestamp: string | null) => void
}>) {
  const data = useMemo(() => {
    const rows = result?.rows ?? []
    const sampleEvery = Math.max(1, Math.ceil(rows.length / 320))
    const sampledRows = rows.filter(
      (_, index) => index % sampleEvery === 0 || index === rows.length - 1,
    )
    return sampledRows.map((row) =>
      Object.fromEntries(
        (result?.columns ?? []).map((column, index) => [
          column,
          row[index] instanceof Uint8Array ? null : (row[index] ?? null),
        ]),
      ),
    )
  }, [result])
  const xKey = result?.columns[0] ?? 'time'
  const xIsTime = /time|date/iu.test(xKey)
  const formatX = (value: string | number): string =>
    xIsTime
      ? chartTime(value)
      : typeof value === 'number'
        ? new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 }).format(value)
        : String(value)
  const series = result?.columns.slice(1) ?? []
  const scaleIds = [...new Set(series.map(scaleForSeries))]
  const visibleScale = scaleIds.find((scale) => scale !== 'boolean') ?? scaleIds[0]
  const hasValues = data.some((row) => series.some((column) => typeof row[column] === 'number'))
  const summaries = series.map((column) => {
    const values = data.flatMap((row) => (typeof row[column] === 'number' ? [row[column]] : []))
    return {
      column,
      mean:
        values.length === 0 ? null : values.reduce((sum, value) => sum + value, 0) / values.length,
      maximum: values.length === 0 ? null : Math.max(...values),
      minimum: values.length === 0 ? null : Math.min(...values),
    }
  })
  const metric = (value: number | null, column: string): string =>
    value === null ? '—' : telemetryValue(value, column)
  const hideStateLine = (column: string): boolean =>
    title.startsWith('Temperatures') && (booleanSeries(column) || /fan/iu.test(column))
  return (
    <article className="catalog-panel drive-telemetry-panel">
      <h2>{title}</h2>
      {hasValues ? (
        <ResponsiveContainer width="100%" height={270}>
          <LineChart
            data={data}
            margin={{ top: 8, right: 14, bottom: 4, left: 2 }}
            {...(xIsTime ? { syncId: 'drive-telemetry', syncMethod: nearestTimeSync } : {})}
            onMouseMove={(state) => onHoverTime?.(typeof state.activeLabel === 'string' ? state.activeLabel : null)}
            onMouseLeave={() => onHoverTime?.(null)}
          >
            <CartesianGrid stroke="#26322f" vertical={false} />
            <XAxis
              dataKey={xKey}
              minTickGap={48}
              stroke="#879491"
              tick={{ fontSize: 11 }}
              tickFormatter={formatX}
            />
            {scaleIds.map((scale) => (
              <YAxis
                key={scale}
                yAxisId={scale}
                hide={scale !== visibleScale}
                domain={scale === 'boolean' ? [0, 1] : ['auto', 'auto']}
                stroke="#879491"
                width={48}
                tick={{ fontSize: 11 }}
              />
            ))}
            <Tooltip content={(props) => <CompactTelemetryTooltip {...props} labelAsTime={xIsTime} xLabel={xKey} />} />
            {series.map((column, index) => (
              <Line
                key={column}
                dataKey={column}
                yAxisId={scaleForSeries(column)}
                dot={false}
                activeDot={!hideStateLine(column)}
                isAnimationActive={false}
                type={booleanSeries(column) || /fan/iu.test(column) ? 'stepAfter' : 'linear'}
                stroke={
                  hideStateLine(column)
                    ? 'transparent'
                    : (telemetryColors[index % telemetryColors.length] ?? '#c9ff43')
                }
                strokeWidth={booleanSeries(column) || /fan/iu.test(column) ? 1 : 1.5}
                connectNulls={!hideStateLine(column)}
              />
            ))}
          </LineChart>
        </ResponsiveContainer>
      ) : (
        <p className="no-data">{emptyMessage}</p>
      )}
      {hasValues && (
        <div className="telemetry-summary" aria-label={`${title} series statistics`}>
          {summaries.map((summary, index) => (
            <div key={summary.column}>
              <i
                aria-hidden="true"
                style={{ background: telemetryColors[index % telemetryColors.length] ?? '#c9ff43' }}
              />
              <b>{summary.column}</b>
              <span>Mean: {metric(summary.mean, summary.column)}</span>
              <span>Max: {metric(summary.maximum, summary.column)}</span>
              <span>Min: {metric(summary.minimum, summary.column)}</span>
            </div>
          ))}
        </div>
      )}
    </article>
  )
}

function ChargingCurveChart({ result }: Readonly<{ result: QueryResult | undefined }>) {
  const data = (result?.rows ?? []).flatMap((row) => {
    const soc = number(row[0])
    const power = number(row[1])
    return soc === null || power === null ? [] : [{ soc, power }]
  })
  return (
    <article className="catalog-panel drive-telemetry-panel">
      <h2>Charging curve</h2>
      {data.length === 0 ? <p className="no-data">No charging curve recorded.</p> : (
        <ResponsiveContainer width="100%" height={270}>
          <LineChart data={data} margin={{ top: 8, right: 18, bottom: 10, left: 2 }}>
            <CartesianGrid stroke="#26322f" vertical={false} />
            <XAxis dataKey="soc" type="number" domain={['dataMin', 'dataMax']} unit="%" stroke="#879491" tick={{ fontSize: 11 }} />
            <YAxis dataKey="power" unit=" kW" stroke="#879491" width={58} tick={{ fontSize: 11 }} />
            <Tooltip formatter={(value) => [`${String(value)} kW`, 'Power']} labelFormatter={(value) => `SOC ${String(value)}%`} />
            <Line dataKey="power" name="Power" dot={false} activeDot isAnimationActive={false} stroke="#c9ff43" strokeWidth={2} />
          </LineChart>
        </ResponsiveContainer>
      )}
    </article>
  )
}

export function TripDashboard({
  bytes,
  settings,
  onSelectDrive,
}: Readonly<{ bytes: Uint8Array; settings: ViewSettings; onSelectDrive?: (driveId: number) => void }>) {
  const tripQueries = useMemo(
    () => [
      `SELECT p.latitude, p.longitude, p.timestamp FROM positions p JOIN drives d ON d.id=p.drive_id WHERE d.status='closed' AND ${timeRangeSql(settings.timeRange, 'd.start_time')} AND p.latitude IS NOT NULL AND p.longitude IS NOT NULL AND (p.id % 25)=0 ORDER BY p.timestamp`,
      `SELECT COALESCE(SUM(d.distance_km),0), COALESCE(SUM(d.duration_min),0), COALESCE(SUM((d.start_range_km-d.end_range_km)*v.efficiency_wh_km),0), COALESCE(SUM(d.distance_km)/NULLIF(SUM(d.duration_min)/60.0,0),0), COALESCE(MAX(d.end_odometer_km)-MIN(d.start_odometer_km),0) FROM drives d JOIN vehicles v ON v.id=d.vehicle_id WHERE d.status='closed' AND ${timeRangeSql(settings.timeRange, 'd.start_time')}`,
      `SELECT COALESCE(SUM(CASE WHEN is_dc_fast_charge=0 THEN (julianday(end_time)-julianday(start_time))*86400 ELSE 0 END),0), COALESCE(SUM(CASE WHEN is_dc_fast_charge=1 THEN (julianday(end_time)-julianday(start_time))*86400 ELSE 0 END),0), COALESCE(SUM(CASE WHEN is_dc_fast_charge=0 THEN charge_energy_added_kwh ELSE 0 END),0), COALESCE(SUM(CASE WHEN is_dc_fast_charge=1 THEN charge_energy_added_kwh ELSE 0 END),0), COALESCE(SUM(cost),0), COALESCE(SUM(CASE WHEN is_dc_fast_charge=1 THEN (julianday(end_time)-julianday(start_time))*24 ELSE 0 END),0), COALESCE(SUM(MAX(COALESCE(charge_energy_added_kwh,0),COALESCE(charge_energy_used_kwh,0))),0) FROM charging_sessions WHERE status='closed' AND ${timeRangeSql(settings.timeRange, 'start_time')}`,
      `WITH events AS (
         SELECT 'drive_start' event,start_time time,start_range_km range FROM drives WHERE status='closed' AND ${timeRangeSql(settings.timeRange, 'start_time')}
         UNION ALL SELECT 'drive_end',COALESCE(end_time,start_time),end_range_km FROM drives WHERE status='closed' AND ${timeRangeSql(settings.timeRange, 'start_time')}
         UNION ALL SELECT 'charge_start',start_time,start_range_km FROM charging_sessions WHERE status='closed' AND ${timeRangeSql(settings.timeRange, 'start_time')}
         UNION ALL SELECT 'charge_end',COALESCE(end_time,start_time),end_range_km FROM charging_sessions WHERE status='closed' AND ${timeRangeSql(settings.timeRange, 'start_time')}
       ), ordered AS (SELECT event,range,LEAD(range) OVER (ORDER BY time) next_range FROM events WHERE range IS NOT NULL), losses AS (
         SELECT CASE WHEN event='drive_start' THEN range-next_range WHEN range-next_range>0 THEN range-next_range ELSE 0 END loss FROM ordered WHERE next_range IS NOT NULL
       ) SELECT COALESCE(SUM(loss)*(SELECT efficiency_wh_km FROM vehicles ORDER BY id LIMIT 1),0) FROM losses`,
      `SELECT id, start_time, COALESCE(start_location, printf('%.4f, %.4f',start_lat,start_lng)), COALESCE(end_location, printf('%.4f, %.4f',end_lat,end_lng)), duration_min, distance_km, start_battery_level, end_battery_level, (start_range_km-end_range_km)*(SELECT efficiency_wh_km FROM vehicles WHERE id=drives.vehicle_id)/NULLIF(distance_km,0) FROM drives WHERE status='closed' AND ${timeRangeSql(settings.timeRange, 'start_time')} ORDER BY start_time DESC`,
      `SELECT start_time "Date", location "Location", CASE WHEN is_dc_fast_charge=1 THEN 'DC' ELSE 'AC' END "Type", ROUND((julianday(end_time)-julianday(start_time))*24*60,1) "Duration (min)", cost "Cost", charge_energy_added_kwh "Energy added (kWh)", charge_energy_used_kwh "Energy used (kWh)", start_battery_level "% Start", end_battery_level "% End" FROM charging_sessions WHERE status='closed' AND ${timeRangeSql(settings.timeRange, 'start_time')} ORDER BY start_time DESC`,
      `SELECT timestamp time, battery_level "Battery (%)", CASE WHEN '${settings.lengthUnit}'='mi' THEN battery_range_km/1.60934 ELSE battery_range_km END "Range (${settings.lengthUnit})" FROM battery_samples WHERE ${timeRangeSql(settings.timeRange, 'timestamp')} ORDER BY timestamp`,
      `SELECT timestamp time, CASE WHEN '${settings.lengthUnit}'='mi' THEN elevation_m*3.28084 ELSE elevation_m END "Elevation" FROM positions WHERE elevation_m IS NOT NULL AND ${timeRangeSql(settings.timeRange, 'timestamp')} ORDER BY timestamp`,
      `SELECT started_at, COALESCE(ended_at,datetime('now')), state FROM states WHERE ${timeRangeSql(settings.timeRange, 'started_at')} UNION ALL SELECT start_time,COALESCE(end_time,datetime('now')),'driving' FROM drives WHERE ${timeRangeSql(settings.timeRange, 'start_time')} UNION ALL SELECT start_time,COALESCE(end_time,datetime('now')),CASE WHEN is_dc_fast_charge=1 THEN 'charging (DC)' ELSE 'charging (AC)' END FROM charging_sessions WHERE ${timeRangeSql(settings.timeRange, 'start_time')} ORDER BY 1`,
    ],
    [settings.lengthUnit, settings.timeRange],
  )
  const { results, error, loading } = useResults(bytes, tripQueries, { lengthUnit: settings.lengthUnit, temperatureUnit: settings.temperatureUnit, timeRange: settings.timeRange, preferredRange: settings.preferredRange, minDistance: settings.minDistance, statisticsPeriod: settings.statisticsPeriod })
  if (loading && results.length === 0) return <DashboardLoading title="Trip" />
  const points = (results[0]?.rows ?? []).flatMap((row) => {
    const latitude = number(row[0])
    const longitude = number(row[1])
    const timestamp = typeof row[2] === 'string' ? row[2] : undefined
    return latitude === null || longitude === null ? [] : [timestamp ? { latitude, longitude, timestamp } : { latitude, longitude }]
  })
  const aggregate = results[1]?.rows[0]
  const charge = results[2]?.rows[0]
  const distanceKm = number(aggregate?.[0]) ?? 0
  const driveMinutes = number(aggregate?.[1]) ?? 0
  const netEnergy = number(aggregate?.[2]) ?? 0
  const averageSpeed = number(aggregate?.[3]) ?? 0
  const acSeconds = number(charge?.[0]) ?? 0
  const dcSeconds = number(charge?.[1]) ?? 0
  const acEnergy = number(charge?.[2]) ?? 0
  const dcEnergy = number(charge?.[3]) ?? 0
  const chargingCost = number(charge?.[4]) ?? 0
  const dcHours = number(charge?.[5]) ?? 0
  // Cost per kWh is billed on max(added, used) - the same basis the Go cost
  // calculator and the Charging stats dashboard use. Using added-only here
  // made the two dashboards disagree about cost per 100 mi.
  const billedEnergy = number(charge?.[6]) ?? 0
  const grossEnergy = number(results[3]?.rows[0]?.[0]) ?? 0
  const shownDistance = distance(distanceKm, settings.lengthUnit)
  // Gross consumption divides by the odometer span, not the summed drive
  // distance - see the Efficiency dashboard's gross panel for why. Falls
  // back to drive distance if the odometer delta is unavailable.
  const odometerKm = number(aggregate?.[4]) ?? 0
  const grossDistance = distance(odometerKm > 0 ? odometerKm : distanceKm, settings.lengthUnit)
  const timeSpent = [
    { name: 'driving', value: driveMinutes * 60, color: '#5794f2' },
    { name: 'charging (AC)', value: acSeconds, color: '#73bf69' },
    { name: 'charging (DC)', value: dcSeconds, color: '#fade2a' },
  ].filter((entry) => entry.value > 0)
  const timeline = (results[8]?.rows ?? []).flatMap((row) => {
    const start = typeof row[0] === 'string' ? timestampDate(row[0]).getTime() : null
    const end = typeof row[1] === 'string' ? timestampDate(row[1]).getTime() : null
    const state = typeof row[2] === 'string' ? row[2] : 'unknown'
    return start === null || end === null ? [] : [{ start, end, state }]
  })
  return (
    <main>
      <Heading title="Trip" note={settings.timeRange === 'all' ? 'All time' : settings.timeRange} />
      {error && <p className="no-data">{error}</p>}
      <section className="trip-grid">
        <article className="catalog-panel drive-route-panel trip-map"><h2>Route</h2><RouteMap points={points} /></article>
        <div className="trip-stats">
          <article><span>Mileage</span><strong>{shownDistance.toFixed(1)} {settings.lengthUnit}</strong></article>
          <article><span>Ø Speed excl. breaks</span><strong>{speed(averageSpeed, settings.lengthUnit).toFixed(1)} {settings.lengthUnit}/h</strong></article>
          <article><span>Ø Speed incl. DC charging</span><strong>{(shownDistance / Math.max(driveMinutes / 60 + dcHours, 0.001)).toFixed(1)} {settings.lengthUnit}/h</strong></article>
          <article><span>Ø Consumption (net)</span><strong>{(netEnergy / Math.max(shownDistance, .001)).toFixed(0)} Wh/{settings.lengthUnit}</strong></article>
          <article><span>Ø Consumption (gross)</span><strong>{(grossEnergy / Math.max(grossDistance, .001)).toFixed(0)} Wh/{settings.lengthUnit}</strong></article>
          <article><span>Total charging cost</span><strong>{chargingCost.toFixed(2)}</strong></article>
          <article><span>Ø Cost per 100 {settings.lengthUnit}</span><strong>{(chargingCost / Math.max(billedEnergy, .001) * (grossEnergy / 1000) / Math.max(shownDistance, .001) * 100).toFixed(2)}</strong></article>
          <article className="energy-bars"><span>Total energy</span><div><i style={{width:`${Math.min(100,grossEnergy/1000/Math.max(grossEnergy/1000,acEnergy+dcEnergy)*100)}%`}} />{(grossEnergy/1000).toFixed(2)} kWh consumed</div><div><i style={{width:`${Math.min(100,(acEnergy+dcEnergy)/Math.max(grossEnergy/1000,acEnergy+dcEnergy)*100)}%`}} />{(acEnergy+dcEnergy).toFixed(2)} kWh added</div></article>
        </div>
        <article className="catalog-panel trip-pie"><h2>Time spent</h2><ResponsiveContainer width="100%" height={260}><PieChart><Pie data={timeSpent} dataKey="value" nameKey="name" outerRadius={88} label={({name,percent}) => `${String(name)} ${((percent ?? 0)*100).toFixed(1)}%`}>{timeSpent.map((entry)=><Cell key={entry.name} fill={entry.color}/>)}</Pie><Tooltip formatter={(value) => `${(Number(value)/3600).toFixed(1)} hours`} /></PieChart></ResponsiveContainer></article>
      </section>
      <StateTimeline spans={timeline} title="States" />
      <h2 className="telemetry-heading">Drives</h2>
      <div className="table-panel trip-table"><table><thead><tr><th>Date</th><th>Start</th><th>Destination</th><th>Duration</th><th>Distance</th><th>% Start</th><th>% End</th><th>Ø Consumption (net)</th></tr></thead><tbody>{(results[4]?.rows ?? []).map((row)=><tr key={text(row[0])}><td><button className="table-link" type="button" onClick={()=>{const id=number(row[0]); if(id!==null) onSelectDrive?.(id)}}>{typeof row[1]==='string' ? timestampDate(row[1]).toLocaleString() : '—'}</button></td><td>{text(row[2])}</td><td>{text(row[3])}</td><td>{text(row[4])} min</td><td>{typeof row[5]==='number' ? `${distance(row[5],settings.lengthUnit).toFixed(1)} ${settings.lengthUnit}`:'—'}</td><td>{text(row[6])}%</td><td>{text(row[7])}%</td><td>{typeof row[8]==='number' ? `${(row[8]*(settings.lengthUnit==='mi'?1.60934:1)).toFixed(0)} Wh/${settings.lengthUnit}`:'—'}</td></tr>)}</tbody></table></div>
      <h2 className="telemetry-heading">Charges</h2><DataTable result={results[5]} />
      <section className="drive-detail-grid"><TelemetryChart result={results[6]} title="Battery Level & Range" /><TelemetryChart result={results[7]} title={`Elevation (${settings.lengthUnit==='mi'?'ft':'m'})`} /></section>
    </main>
  )
}

export function VisitedDashboard({
  bytes,
  settings,
}: Readonly<{ bytes: Uint8Array; settings: ViewSettings }>) {
  const visitedQueries = useMemo(
    () => [
      `SELECT COALESCE(end_location, printf('%.4f, %.4f',end_lat,end_lng)) "Location", COUNT(*) "Visits", ROUND(MAX(end_lat),5) latitude, ROUND(MAX(end_lng),5) longitude, MAX(end_time) "Last visited" FROM drives WHERE status='closed' AND ${timeRangeSql(settings.timeRange, 'end_time')} GROUP BY COALESCE(end_location, printf('%.4f, %.4f',end_lat,end_lng)) ORDER BY COUNT(*) DESC`,
      // Mileage from the odometer delta rather than summed drive
      // distance, matching TeslaMate: the odometer is the car's own
      // record and includes anything teslalog missed.
      `SELECT COALESCE(CASE WHEN '${settings.lengthUnit}'='mi' THEN (MAX(end_odometer_km)-MIN(start_odometer_km))/1.60934 ELSE MAX(end_odometer_km)-MIN(start_odometer_km) END,0)
       FROM drives WHERE status='closed' AND ${timeRangeSql(settings.timeRange, 'start_time')}`,
      `SELECT COALESCE(SUM(charge_energy_added_kwh),0),
              COALESCE(SUM(MAX(COALESCE(charge_energy_added_kwh,0),COALESCE(charge_energy_used_kwh,0))),0),
              SUM(charge_energy_added_kwh)*100.0/NULLIF(SUM(MAX(COALESCE(charge_energy_added_kwh,0),COALESCE(charge_energy_used_kwh,0))),0),
              COALESCE(SUM(cost),0)
       FROM charging_sessions WHERE status='closed' AND charge_energy_added_kwh > 0.01
         AND ${timeRangeSql(settings.timeRange, 'start_time')}`,
    ],
    [settings.timeRange, settings.lengthUnit],
  )
  const { results, error, loading } = useResults(bytes, visitedQueries)
  if (loading && results.length === 0) return <DashboardLoading title="Visited" />
  const points = (results[0]?.rows ?? []).flatMap((row) => {
    const latitude = number(row[2])
    const longitude = number(row[3])
    return latitude === null || longitude === null ? [] : [{ latitude, longitude }]
  })
  return (
    <main>
      <Heading
        title="Visited"
        note={settings.timeRange === 'all' ? 'All time' : settings.timeRange}
      />
      {error && <p className="no-data">{error}</p>}
      <section className="charging-stat-grid">
        {[
          ['Mileage', `${(number(results[1]?.rows[0]?.[0]) ?? 0).toFixed(0)} ${settings.lengthUnit}`],
          ['Energy added', `${(number(results[2]?.rows[0]?.[0]) ?? 0).toFixed(1)} kWh`],
          ['Energy used', `${(number(results[2]?.rows[0]?.[1]) ?? 0).toFixed(1)} kWh`],
          ['Charging efficiency', `${(number(results[2]?.rows[0]?.[2]) ?? 0).toFixed(1)}%`],
          ['Total charging cost', (number(results[2]?.rows[0]?.[3]) ?? 0).toFixed(2)],
        ].map(([label, value]) => (
          <article key={label}>
            <span>{label}</span>
            <strong>{value}</strong>
          </article>
        ))}
      </section>
      <section className="catalog-panel route-panel">
        <h2>Visited places</h2>
        <LocalPlot points={points} />
      </section>
      <DataTable result={results[0]} />
    </main>
  )
}

const databaseQueries = [
  `SELECT 'vehicles' "Table", COUNT(*) "Rows" FROM vehicles UNION ALL SELECT 'states',COUNT(*) FROM states UNION ALL SELECT 'drives',COUNT(*) FROM drives UNION ALL SELECT 'positions',COUNT(*) FROM positions UNION ALL SELECT 'charging_sessions',COUNT(*) FROM charging_sessions UNION ALL SELECT 'charging_samples',COUNT(*) FROM charging_samples UNION ALL SELECT 'battery_samples',COUNT(*) FROM battery_samples UNION ALL SELECT 'software_updates',COUNT(*) FROM software_updates`,
  `SELECT tbl_name "Table", name "Index", COALESCE(sql, 'automatic') "Definition" FROM sqlite_schema WHERE type='index' ORDER BY tbl_name, name`,
  // TeslaMate's Mileage / Stats / Software / Incomplete Data cards. The
  // Postgres-specific half of its Database information dashboard
  // (pg_stat_statements, shared_buffers, server version) has no
  // equivalent here and is not ported: teslalog embeds SQLite, so there
  // is no server to report on.
  `SELECT (SELECT COALESCE(MAX(end_odometer_km),0) FROM drives WHERE status='closed'),
          (SELECT COUNT(*) FROM drives WHERE status='closed'),
          (SELECT COUNT(*) FROM charging_sessions WHERE status='closed'),
          (SELECT COALESCE(firmware_version,'unknown') FROM vehicles ORDER BY id LIMIT 1),
          (SELECT COUNT(*) FROM drives WHERE status != 'closed'),
          (SELECT COUNT(*) FROM charging_sessions WHERE status != 'closed'),
          (SELECT MIN(start_time) FROM drives),
          (SELECT MAX(COALESCE(end_time, start_time)) FROM drives)`,
]
export function DatabaseInformationDashboard({
  bytes,
  settings,
}: Readonly<{ bytes: Uint8Array; settings: ViewSettings }>) {
  const { results, error, loading } = useResults(bytes, databaseQueries)
  if (loading && results.length === 0) return <DashboardLoading title="Database information" />
  const row = results[2]?.rows[0]
  const count = (index: number): number => number(row?.[index]) ?? 0
  const incomplete = count(4) + count(5)
  const odometer = distance(count(0), settings.lengthUnit)
  const cards: readonly (readonly [string, string])[] = [
    ['Odometer', `${odometer.toFixed(0)} ${settings.lengthUnit}`],
    ['Drives logged', String(count(1))],
    ['Charges logged', String(count(2))],
    ['Firmware', text(row?.[3])],
    // Named the way TeslaMate names it. Zero is the state to expect; a
    // non-zero count means some totals in this viewer are quietly short,
    // because everything sums over closed records only.
    ['Incomplete data', incomplete === 0 ? 'none' : `${incomplete} unclosed`],
    ['Logging since', row?.[6] === null || row?.[6] === undefined ? '—' : timestampDate(String(row[6])).toLocaleDateString()],
  ]
  return (
    <main>
      <Heading title="Database information" note="Uploaded SQLite snapshot" />
      {error && <p className="no-data">{error}</p>}
      <section className="charging-stat-grid">
        {cards.map(([label, value]) => (
          <article key={label}>
            <span>{label}</span>
            <strong>{value}</strong>
          </article>
        ))}
      </section>
      <h2 className="telemetry-heading">Row counts</h2>
      <DataTable result={results[0]} />
      <h2 className="telemetry-heading">Indexes</h2>
      <DataTable result={results[1]} />
    </main>
  )
}

export function OverviewDashboard({ bytes, settings }: Readonly<{ bytes: Uint8Array; settings: ViewSettings }>) {
  const queries = useMemo(() => [
    `SELECT v.display_name,COALESCE(v.marketing_name,v.model,''),COALESCE(v.firmware_version,''),COALESCE((SELECT state FROM states ORDER BY started_at DESC LIMIT 1),'unknown'),
      (SELECT battery_level FROM battery_samples ORDER BY timestamp DESC LIMIT 1),
      (SELECT CASE WHEN '${settings.lengthUnit}'='mi' THEN battery_range_km/1.60934 ELSE battery_range_km END FROM battery_samples ORDER BY timestamp DESC LIMIT 1),
      (SELECT CASE WHEN '${settings.lengthUnit}'='mi' THEN odometer_km/1.60934 ELSE odometer_km END FROM positions WHERE odometer_km IS NOT NULL ORDER BY timestamp DESC LIMIT 1),
      (SELECT CASE WHEN '${settings.temperatureUnit}'='F' THEN driver_temp_setting_c*9.0/5+32 ELSE driver_temp_setting_c END FROM positions WHERE driver_temp_setting_c IS NOT NULL ORDER BY timestamp DESC LIMIT 1),
      (SELECT CASE WHEN '${settings.temperatureUnit}'='F' THEN outside_temp_c*9.0/5+32 ELSE outside_temp_c END FROM positions WHERE outside_temp_c IS NOT NULL ORDER BY timestamp DESC LIMIT 1),
      (SELECT CASE WHEN '${settings.temperatureUnit}'='F' THEN inside_temp_c*9.0/5+32 ELSE inside_temp_c END FROM positions WHERE inside_temp_c IS NOT NULL ORDER BY timestamp DESC LIMIT 1),
      (SELECT charger_voltage FROM charging_samples ORDER BY timestamp DESC LIMIT 1),(SELECT charger_power_kw FROM charging_samples ORDER BY timestamp DESC LIMIT 1)
     FROM vehicles v ORDER BY v.id LIMIT 1`,
    `SELECT COALESCE(SUM((d.start_range_km-d.end_range_km)*v.efficiency_wh_km),0),COALESCE(SUM(d.distance_km),0),COALESCE(MAX(d.end_odometer_km)-MIN(d.start_odometer_km),0) FROM drives d JOIN vehicles v ON v.id=d.vehicle_id WHERE ${timeRangeSql(settings.timeRange,'d.start_time')}`,
    `WITH events AS (SELECT 'drive_start' event,start_time time,start_range_km range FROM drives WHERE ${timeRangeSql(settings.timeRange,'start_time')} UNION ALL SELECT 'drive_end',COALESCE(end_time,start_time),end_range_km FROM drives WHERE ${timeRangeSql(settings.timeRange,'start_time')} UNION ALL SELECT 'charge_start',start_time,start_range_km FROM charging_sessions WHERE ${timeRangeSql(settings.timeRange,'start_time')} UNION ALL SELECT 'charge_end',COALESCE(end_time,start_time),end_range_km FROM charging_sessions WHERE ${timeRangeSql(settings.timeRange,'start_time')}), ordered AS (SELECT event,range,LEAD(range) OVER (ORDER BY time) next_range FROM events WHERE range IS NOT NULL), losses AS (SELECT CASE WHEN event='drive_start' THEN range-next_range WHEN range-next_range>0 THEN range-next_range ELSE 0 END loss FROM ordered WHERE next_range IS NOT NULL) SELECT COALESCE(SUM(loss)*(SELECT efficiency_wh_km FROM vehicles LIMIT 1),0) FROM losses`,
    `SELECT timestamp time,battery_level "SOC (%)" FROM battery_samples WHERE ${timeRangeSql(settings.timeRange,'timestamp')} ORDER BY timestamp`,
    `SELECT timestamp time,charger_power_kw "Power (kW)",battery_heater_on "Battery heater",charger_actual_current "Current (A)",charge_energy_added_kwh "Energy added (kWh)",charger_voltage "Charging voltage (V)" FROM charging_samples WHERE ${timeRangeSql(settings.timeRange,'timestamp')} ORDER BY timestamp`,
    `SELECT started_at,COALESCE(ended_at,datetime('now')),state FROM states WHERE ${timeRangeSql(settings.timeRange,'started_at')} ORDER BY started_at`,
  ], [settings])
  const {results,error,loading}=useResults(bytes,queries,{ lengthUnit: settings.lengthUnit, temperatureUnit: settings.temperatureUnit, timeRange: settings.timeRange, preferredRange: settings.preferredRange, minDistance: settings.minDistance, statisticsPeriod: settings.statisticsPeriod })
  if (loading && results.length === 0) return <DashboardLoading title="Overview" />
  const row=results[0]?.rows[0]
  const value=(index:number,digits=0):string=>{const current=number(row?.[index]); return current===null?'—':current.toFixed(digits)}
  const net=number(results[1]?.rows[0]?.[0])??0
  const distanceKm=number(results[1]?.rows[0]?.[1])??0
  const gross=number(results[2]?.rows[0]?.[0])??0
  const shownDistance=distance(distanceKm,settings.lengthUnit)
  // Gross divides by odometer span (see the Efficiency gross panel);
  // net keeps summed drive distance.
  const odometerKm=number(results[1]?.rows[0]?.[2])??0
  const grossDistance=distance(odometerKm>0?odometerKm:distanceKm,settings.lengthUnit)
  const states=(results[5]?.rows??[]).flatMap((stateRow)=>{
    if(typeof stateRow[0]!=='string'||typeof stateRow[1]!=='string'||typeof stateRow[2]!=='string')return[]
    return[{start:timestampDate(stateRow[0]).getTime(),end:timestampDate(stateRow[1]).getTime(),state:stateRow[2]}]
  })
  return <main>
    <Heading title={text(row?.[0])} note={text(row?.[1])}/>
    {error&&<p className="no-data">{error}</p>}
    <section className="overview-stat-grid">
      <article><span>Battery level</span><strong>{value(4)}%</strong></article><article><span>Charging voltage</span><strong>{value(10)} V</strong></article><article><span>Charging power</span><strong>{value(11)} kW</strong></article>
      <article><span>Ø Consumption (net)</span><strong>{(net/Math.max(shownDistance,.001)).toFixed(0)} Wh/{settings.lengthUnit}</strong></article><article><span>Ø Consumption (gross)</span><strong>{(gross/Math.max(grossDistance,.001)).toFixed(0)} Wh/{settings.lengthUnit}</strong></article><article><span>Total distance logged</span><strong>{shownDistance.toFixed(1)} {settings.lengthUnit}</strong></article>
      <article><span>Range</span><strong>{value(5)} {settings.lengthUnit}</strong></article><article><span>Firmware</span><strong>{text(row?.[2])}</strong></article><article><span>Odometer</span><strong>{value(6)} {settings.lengthUnit}</strong></article>
      <article><span>Driver temp</span><strong>{value(7)} °{settings.temperatureUnit}</strong></article><article><span>Outside temp</span><strong>{value(8)} °{settings.temperatureUnit}</strong></article><article><span>Inside temp</span><strong>{value(9)} °{settings.temperatureUnit}</strong></article>
    </section>
    <section className="drive-detail-grid"><TelemetryChart result={results[3]} title="Charge Level"/><TelemetryChart result={results[4]} title="Charging Details"/></section>
    <StateTimeline spans={states} title="States" />
  </main>
}

export function BatteryHealthDashboard({
  bytes,
  settings,
}: Readonly<{ bytes: Uint8Array; settings: ViewSettings }>) {
  const healthQueries = useMemo(() => [
    `WITH candidates AS (
       SELECT ROUND(charge_energy_added_kwh/NULLIF(end_range_km-start_range_km,0),3)*100 efficiency, COUNT(*) frequency
       FROM charging_sessions WHERE (julianday(end_time)-julianday(start_time))*1440>10 AND end_battery_level<=95 AND charge_energy_added_kwh>0
         AND start_range_km IS NOT NULL AND end_range_km>start_range_km GROUP BY 1 ORDER BY 2 DESC LIMIT 1
     ), eff AS (
       SELECT COALESCE((SELECT efficiency FROM candidates),(SELECT efficiency_wh_km/10.0 FROM vehicles LIMIT 1)) value
     ), last_samples AS (
       SELECT cs.charging_session_id,MAX(cs.timestamp) timestamp FROM charging_samples cs
       JOIN charging_sessions s ON s.id=cs.charging_session_id CROSS JOIN eff
       WHERE s.end_time IS NOT NULL AND s.charge_energy_added_kwh>=eff.value AND cs.usable_battery_level>0 GROUP BY 1
     ), last_caps AS (
       SELECT cs.timestamp, cs.range_km*eff.value/NULLIF(cs.usable_battery_level,0) capacity FROM last_samples l
       JOIN charging_samples cs ON cs.charging_session_id=l.charging_session_id AND cs.timestamp=l.timestamp CROSS JOIN eff
       WHERE cs.usable_battery_level>0 AND cs.range_km>0
     ),
     -- Both the "new" and the "now" capacity come from last_caps, one
     -- end-of-charge sample per session. They used to be drawn from two
     -- different populations - a MAX over end-of-charge samples against an
     -- AVG over every mid-charge sample - which is biased by construction:
     -- with six charges logged it reported the battery as 0.6 kWh LARGER
     -- than new while the range panel beside it showed range lost.
     recent AS (SELECT capacity FROM last_caps ORDER BY timestamp DESC LIMIT 100),
     daily_ranges AS (
       SELECT SUM(range_km)*100.0/NULLIF(SUM(usable_battery_level),0) projected_range
       FROM charging_samples WHERE usable_battery_level>0 AND range_km>0 GROUP BY date(timestamp)
     ), current_ranges AS (
       SELECT timestamp,range_km*100.0/NULLIF(usable_battery_level,0) projected_range FROM positions WHERE usable_battery_level>0 AND range_km>0
       UNION ALL SELECT timestamp,range_km*100.0/NULLIF(usable_battery_level,0) FROM charging_samples WHERE usable_battery_level>0 AND range_km>0
     ) SELECT (SELECT value FROM eff),(SELECT MAX(capacity) FROM last_caps),(SELECT AVG(capacity) FROM recent),
       (SELECT MAX(projected_range) FROM daily_ranges),(SELECT projected_range FROM current_ranges ORDER BY timestamp DESC LIMIT 1)`,
    `SELECT COALESCE(SUM(distance_km),0),COALESCE(MAX(end_odometer_km)-MIN(start_odometer_km),0),
       COALESCE(MAX(end_odometer_km),0),COALESCE(MAX(end_odometer_km)-MIN(start_odometer_km)-SUM(distance_km),0)
     FROM drives WHERE status='closed'`,
    `SELECT COUNT(*),COALESCE(SUM(charge_energy_added_kwh),0),
       COALESCE(SUM(MAX(COALESCE(charge_energy_added_kwh,0),COALESCE(charge_energy_used_kwh,0))),0),
       COALESCE(SUM(CASE WHEN is_dc_fast_charge=0 THEN MAX(COALESCE(charge_energy_added_kwh,0),COALESCE(charge_energy_used_kwh,0)) ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN is_dc_fast_charge=1 THEN MAX(COALESCE(charge_energy_added_kwh,0),COALESCE(charge_energy_used_kwh,0)) ELSE 0 END),0)
     FROM charging_sessions WHERE status='closed' AND charge_energy_added_kwh>0.01`,
    `SELECT usable_battery_level FROM (
       SELECT timestamp,usable_battery_level FROM positions WHERE usable_battery_level IS NOT NULL
       UNION ALL SELECT timestamp,usable_battery_level FROM charging_samples WHERE usable_battery_level IS NOT NULL
     ) ORDER BY timestamp DESC LIMIT 1`,
    `WITH candidates AS (
       SELECT ROUND(charge_energy_added_kwh/NULLIF(end_range_km-start_range_km,0),3)*100 efficiency,COUNT(*) frequency
       FROM charging_sessions WHERE (julianday(end_time)-julianday(start_time))*1440>10 AND end_battery_level<=95 AND charge_energy_added_kwh>0
         AND start_range_km IS NOT NULL AND end_range_km>start_range_km GROUP BY 1 ORDER BY 2 DESC LIMIT 1
     ), eff AS (SELECT COALESCE((SELECT efficiency FROM candidates),(SELECT efficiency_wh_km*100 FROM vehicles LIMIT 1)) value),
     last_samples AS (
       SELECT s.id,s.end_time,MAX(cs.timestamp) timestamp FROM charging_sessions s
       JOIN charging_samples cs ON cs.charging_session_id=s.id WHERE s.end_time IS NOT NULL GROUP BY s.id
     ) SELECT (SELECT MAX(d.end_odometer_km) FROM drives d WHERE d.end_time<=l.end_time) "Odometer (km)",
       cs.range_km*eff.value/NULLIF(cs.usable_battery_level,0) "Capacity (kWh)"
     FROM last_samples l JOIN charging_samples cs ON cs.charging_session_id=l.id AND cs.timestamp=l.timestamp CROSS JOIN eff
     WHERE cs.usable_battery_level>0 AND cs.range_km>0 ORDER BY l.end_time`,
  ], [])
  const { results, error, loading } = useResults(bytes, healthQueries)
  if (loading && results.length === 0) return <DashboardLoading title="Battery health" />
  const aux = results[0]?.rows[0]
  const drive = results[1]?.rows[0]
  const charge = results[2]?.rows[0]
  const efficiency = number(aux?.[0])
  const maximumCapacity = number(aux?.[1])
  const currentCapacity = number(aux?.[2])
  const maximumRange = number(aux?.[3])
  const currentRange = number(aux?.[4])
  const soc = number(results[3]?.rows[0]?.[0])
  const decimal = (candidate: number | null, digits = 1): string => candidate === null ? '—' : candidate.toFixed(digits)
  const shownDistance = (candidate: number | null): string => candidate === null ? '—' : distance(candidate, settings.lengthUnit).toFixed(1)
  const degradation = currentCapacity === null || maximumCapacity === null ? null : Math.max(0,100-currentCapacity*100/maximumCapacity)
  const pieData = [{ name:'AC', value:number(charge?.[3])??0, color:'#73bf69' },{ name:'DC', value:number(charge?.[4])??0, color:'#fade2a' }]
  const capacityResult: QueryResult | undefined = results[4] ? {
    columns: [`Odometer (${settings.lengthUnit})`, 'Capacity (kWh)'],
    rows: results[4].rows.map((row) => [typeof row[0] === 'number' ? distance(row[0],settings.lengthUnit) : (row[0] ?? null),row[1] ?? null]),
  } : undefined
  return (
    <main>
      <Heading title="Battery health" note="TeslaMate capacity model" />
      {error && <p className="no-data">{error}</p>}
      <section className="battery-health-grid">
        <article><h2>Battery Capacity</h2><strong>{decimal(maximumCapacity)} kWh</strong><span>Usable (new)</span><strong>{decimal(currentCapacity)} kWh</strong><span>Usable (now)</span><small>Difference: {decimal(currentCapacity===null||maximumCapacity===null?null:currentCapacity-maximumCapacity)} kWh</small></article>
        <article><h2>Ranges [rated]</h2><strong>{shownDistance(maximumRange)} {settings.lengthUnit}</strong><span>Max range (new)</span><strong>{shownDistance(currentRange)} {settings.lengthUnit}</strong><span>Max range (now)</span><small>Range lost: {shownDistance(maximumRange===null||currentRange===null?null:maximumRange-currentRange)} {settings.lengthUnit}</small></article>
        <article><h2>Drive Stats</h2><strong>{shownDistance(number(drive?.[0]))} {settings.lengthUnit}</strong><span>Logged</span><small>Mileage {shownDistance(number(drive?.[1]))} · Odometer {shownDistance(number(drive?.[2]))} · Data lost {shownDistance(number(drive?.[3]))} {settings.lengthUnit}</small></article>
        <article><h2>Estimated Degradation</h2><strong>{decimal(degradation)}%</strong></article>
        <article><h2>Battery Health</h2><strong>{decimal(degradation===null?null:100-degradation)}%</strong></article>
        <article><h2>Charging Stats</h2><strong>{decimal(number(charge?.[0]),0)}</strong><span># of Charges</span><small>{Math.floor((number(charge?.[1])??0)/Math.max(maximumCapacity??1,1))} cycles · {decimal(number(charge?.[1]))} kWh added · {decimal(number(charge?.[2]))} kWh used</small></article>
        <article><h2>Current SOC</h2><strong>{decimal(soc)}%</strong></article>
        <article><h2>Efficiency</h2><strong>{decimal(efficiency===null?null:efficiency*10*(settings.lengthUnit==='mi'?1.60934:1),0)} Wh/{settings.lengthUnit}</strong></article>
        <article><h2>Current Stored Energy</h2><strong>{decimal(currentCapacity===null||soc===null?null:currentCapacity*soc/100)} kWh</strong></article>
      </section>
      <section className="drive-detail-grid">
        <article className="catalog-panel trip-pie"><h2>AC/DC - Energy Used</h2><ResponsiveContainer width="100%" height={280}><PieChart><Pie data={pieData} dataKey="value" nameKey="name" innerRadius="42%" outerRadius="76%" label>{pieData.map(item=><Cell key={item.name} fill={item.color}/>)}</Pie><Tooltip/></PieChart></ResponsiveContainer></article>
        <TelemetryChart result={capacityResult} title="Battery Capacity by Mileage" />
      </section>
    </main>
  )
}

export function ProjectedRangeDashboard({
  bytes,
  settings,
}: Readonly<{ bytes: Uint8Array; settings: ViewSettings }>) {
  const projectedQueries = useMemo(
    () => [
      `SELECT (CASE WHEN '$length_unit'='mi' THEN odometer_km/1.60934 ELSE odometer_km END) AS "Mileage ($length_unit)",
        (CASE WHEN '$length_unit'='mi' THEN range_km*100.0/NULLIF(battery_level,0)/1.60934 ELSE range_km*100.0/NULLIF(battery_level,0) END) AS "Projected range ($length_unit)"
       FROM positions WHERE battery_level>=20 AND range_km>0 AND odometer_km IS NOT NULL AND ${timeRangeSql(settings.timeRange, 'timestamp')} ORDER BY timestamp`,
      `SELECT battery_level AS "Battery level (%)",
        (CASE WHEN '$length_unit'='mi' THEN range_km*100.0/NULLIF(battery_level,0)/1.60934 ELSE range_km*100.0/NULLIF(battery_level,0) END) AS "Projected range ($length_unit)"
       FROM positions WHERE battery_level>=20 AND range_km>0 AND ${timeRangeSql(settings.timeRange, 'timestamp')} ORDER BY timestamp`,
      `SELECT (CASE WHEN '$temp_unit'='F' THEN outside_temp_c*9.0/5+32 ELSE outside_temp_c END) AS "Outdoor temp (°$temp_unit)",
        (CASE WHEN '$length_unit'='mi' THEN range_km*100.0/NULLIF(battery_level,0)/1.60934 ELSE range_km*100.0/NULLIF(battery_level,0) END) AS "Projected range ($length_unit)"
       FROM positions WHERE battery_level>=20 AND range_km>0 AND outside_temp_c IS NOT NULL AND ${timeRangeSql(settings.timeRange, 'timestamp')} ORDER BY timestamp`,
    ],
    [settings.timeRange],
  )
  const { results, error, loading } = useResults(bytes, projectedQueries, { lengthUnit: settings.lengthUnit, temperatureUnit: settings.temperatureUnit, timeRange: settings.timeRange, preferredRange: settings.preferredRange, minDistance: settings.minDistance, statisticsPeriod: settings.statisticsPeriod })
  if (loading && results.length === 0) return <DashboardLoading title="Projected range" />
  return (
    <main>
      <Heading title="Projected range" note={settings.timeRange.toUpperCase()} />
      {error && <p className="no-data">{error}</p>}
      <section className="projected-range-grid">
        <TelemetryChart result={results[0]} title="Projected Range - Mileage" />
        <TelemetryChart result={results[1]} title="Projected Range - Battery Level" />
        <TelemetryChart result={results[2]} title="Projected Range - Outdoor Temp" />
      </section>
    </main>
  )
}

function DistributionPie({ result, unit }: Readonly<{ result: QueryResult | undefined; unit: string }>) {
  const data = (result?.rows ?? []).flatMap((row) => typeof row[0] === 'string' && typeof row[1] === 'number' ? [{name:row[0],value:row[1]}] : [])
  return data.length === 0 ? <p className="no-data">No matching charging data.</p> : <ResponsiveContainer width="100%" height={270}><PieChart><Pie data={data} dataKey="value" nameKey="name" innerRadius="42%" outerRadius="76%" label={({name,value})=>`${String(name)} ${Number(value).toFixed(1)} ${unit}`}>{data.map(item=><Cell key={item.name} fill={item.name==='DC'?'#fade2a':'#73bf69'}/>)}</Pie><Tooltip formatter={(value)=>[`${Number(value).toFixed(1)} ${unit}`,'']}/></PieChart></ResponsiveContainer>
}

export function ChargingStatsDashboard({ bytes, settings }: Readonly<{ bytes: Uint8Array; settings: ViewSettings }>) {
  const queries = useMemo(() => {
    const filtered = timeRangeSql(settings.timeRange,'end_time')
    return [
      `WITH c AS (SELECT * FROM charging_sessions WHERE status='closed' AND ${filtered}), d AS (SELECT d.*,v.efficiency_wh_km FROM drives d JOIN vehicles v ON v.id=d.vehicle_id WHERE d.status='closed' AND ${timeRangeSql(settings.timeRange,'d.start_time')}),
       events AS (SELECT 'drive_start' event,start_time time,start_range_km range FROM d UNION ALL SELECT 'drive_end',COALESCE(end_time,start_time),end_range_km FROM d UNION ALL SELECT 'charge_start',start_time,start_range_km FROM c UNION ALL SELECT 'charge_end',COALESCE(end_time,start_time),end_range_km FROM c),
       ordered AS (SELECT event,range,LEAD(range) OVER (ORDER BY time) next_range FROM events WHERE range IS NOT NULL), losses AS (SELECT CASE WHEN event='drive_start' THEN range-next_range WHEN range-next_range>0 THEN range-next_range ELSE 0 END loss FROM ordered WHERE next_range IS NOT NULL),
       gross AS (SELECT SUM(loss)*(SELECT efficiency_wh_km FROM vehicles LIMIT 1) energy,(SELECT MAX(end_odometer_km)-MIN(start_odometer_km) FROM d) distance FROM losses)
       SELECT COUNT(*),COALESCE(SUM(charge_energy_added_kwh),0),COALESCE(SUM(CASE WHEN is_dc_fast_charge=1 AND (LOWER(location) LIKE '%supercharger%' OR location IS NULL) THEN cost ELSE 0 END),0),COALESCE(SUM(cost),0),
       COALESCE(SUM(cost),0)/NULLIF(SUM(charge_energy_added_kwh),0)*(SELECT (energy/1000.0)/NULLIF(CASE WHEN '${settings.lengthUnit}'='mi' THEN distance/1.60934 ELSE distance END,0)*100 FROM gross),
       SUM(cost)/NULLIF(SUM(MAX(COALESCE(charge_energy_added_kwh,0),COALESCE(charge_energy_used_kwh,0))),0),
       SUM(CASE WHEN is_dc_fast_charge=1 THEN cost ELSE 0 END)/NULLIF(SUM(CASE WHEN is_dc_fast_charge=1 THEN MAX(COALESCE(charge_energy_added_kwh,0),COALESCE(charge_energy_used_kwh,0)) ELSE 0 END),0),
       SUM(CASE WHEN is_dc_fast_charge=0 THEN cost ELSE 0 END)/NULLIF(SUM(CASE WHEN is_dc_fast_charge=0 THEN MAX(COALESCE(charge_energy_added_kwh,0),COALESCE(charge_energy_used_kwh,0)) ELSE 0 END),0) FROM c`,
      `SELECT end_time time,start_battery_level "Start SOC",end_battery_level "End SOC" FROM charging_sessions WHERE status='closed' AND ${filtered} ORDER BY end_time`,
      `SELECT CASE WHEN is_dc_fast_charge=1 THEN 'DC' ELSE 'AC' END,SUM(MAX(COALESCE(charge_energy_added_kwh,0),COALESCE(charge_energy_used_kwh,0))) FROM charging_sessions WHERE status='closed' AND ${filtered} GROUP BY 1`,
      `SELECT CASE WHEN is_dc_fast_charge=1 THEN 'DC' ELSE 'AC' END,SUM((julianday(end_time)-julianday(start_time))*24) FROM charging_sessions WHERE status='closed' AND ${filtered} GROUP BY 1`,
      `SELECT location,AVG(latitude),AVG(longitude),SUM(charge_energy_added_kwh),COUNT(*) FROM charging_sessions WHERE status='closed' AND latitude IS NOT NULL AND longitude IS NOT NULL AND ${filtered} GROUP BY location ORDER BY 4 DESC`,
      `SELECT battery_level "SOC (%)",AVG(charger_power_kw) "Power (kW)" FROM charging_samples WHERE fast_charger_present=1 AND charger_power_kw>0 AND ${timeRangeSql(settings.timeRange,'timestamp')} GROUP BY battery_level ORDER BY battery_level`,
      `SELECT ROUND(end_battery_level/5.0)*5 "SOC",COUNT(*) "Charges" FROM charging_sessions WHERE status='closed' AND ${filtered} GROUP BY 1 ORDER BY 1`,
      `SELECT ROUND(start_battery_level/5.0)*5 "SOC",COUNT(*) "Discharges" FROM charging_sessions WHERE status='closed' AND ${filtered} GROUP BY 1 ORDER BY 1`,
      `SELECT location "Location",SUM(charge_energy_added_kwh) "Energy added (kWh)" FROM charging_sessions WHERE status='closed' AND ${filtered} GROUP BY location ORDER BY 2 DESC LIMIT 17`,
      `SELECT location "Location",SUM(cost) "Cost" FROM charging_sessions WHERE status='closed' AND cost IS NOT NULL AND ${filtered} GROUP BY location ORDER BY 2 DESC LIMIT 17`,
    ]
  },[settings.timeRange])
  const {results,error,loading}=useResults(bytes,queries)
  if (loading && results.length === 0) return <DashboardLoading title="Charging stats" />
  const metric=results[0]?.rows[0]
  const money=(value:QueryValue|undefined):string=>typeof value==='number'?value.toFixed(2):'—'
  const cards=[['# of Charges',number(metric?.[0])?.toFixed(0)??'—'],['Total Energy added',`${money(metric?.[1])} kWh`],['SuC Charging Cost',money(metric?.[2])],['Total Charging Cost',money(metric?.[3])],[`Ø Cost per 100 ${settings.lengthUnit}`,money(metric?.[4])],['Ø Cost per kWh',money(metric?.[5])],['Ø Cost per kWh DC',money(metric?.[6])],['Ø Cost per kWh AC',money(metric?.[7])]]
  return <main><Heading title="Charging stats" note={settings.timeRange.toUpperCase()}/>{error&&<p className="no-data">{error}</p>}<section className="charging-stat-grid">{cards.map(([label,value])=><article key={label}><span>{label}</span><strong>{value}</strong></article>)}</section><section className="charging-analytics-grid"><TelemetryChart result={results[1]} title="Charge Heatmap"/><TelemetryChart result={results[1]} title="Charge Delta"/><article className="catalog-panel"><h2>AC/DC - Energy Used</h2><DistributionPie result={results[2]} unit="kWh"/></article><article className="catalog-panel"><h2>Charging heat map by kWh</h2><ChargingSitesMap result={results[4]}/></article><article className="catalog-panel"><h2>AC/DC - Duration</h2><DistributionPie result={results[3]} unit="hours"/></article><TelemetryChart result={results[5]} title="DC Charging Curve"/><article className="catalog-panel"><h2>Charge Stats</h2><DataTable result={results[6]}/></article><article className="catalog-panel"><h2>Discharge Stats</h2><DataTable result={results[7]}/></article><article className="catalog-panel"><h2>Top Charging Stations (Charged)</h2><DataTable result={results[8]}/></article><article className="catalog-panel"><h2>Top Charging Stations (Cost)</h2><DataTable result={results[9]}/></article></section></main>
}

// The per-drive figures shown as cards, in display order. Titles must
// match grafana/teslalog-drive-details.json exactly - that file is the
// single definition of each query, shared with Grafana.
const driveStatTitles = [
  'Distance ($length_unit)',
  'Drive duration (min)',
  'Battery start → end',
  'Max speed ($length_unit/h)',
  'Ø speed ($length_unit/h)',
  'Odometer from → to ($length_unit)',
  'Energy consumed (net) (kWh)',
  'Consumption (net) (Wh/$length_unit)',
  'Energy recovered (kWh)',
  'Energy drawn (kWh)',
  'Elevation up / down',
] as const

export function DriveDetailsDashboard({
  bytes,
  driveId,
  onBack,
  settings,
}: Readonly<{
  bytes: Uint8Array
  driveId: number
  onBack: () => void
  settings: ViewSettings
}>) {
  const [hoverTime, setHoverTime] = useState<string | null>(null)
  const definition = dashboardCatalog.find((dashboard) => dashboard.key === 'drive-details')
  // Catalog panels are picked by title, not by position. They used to be
  // sliced by index, so adding a panel to the catalog silently handed the
  // route map the odometer query.
  const catalogQuery = (title: string): string =>
    definition?.panels.find((panel) => panel.title === title)?.queries[0] ?? 'SELECT NULL AS value'
  // Every telemetry query below filters to rows that actually carry its
  // own columns. The positions table is written by two paths with
  // different coverage - the stream supplies speed and elevation, polls
  // supply temperature and climate - so on a real 4096-position drive
  // only 478 rows hold a temperature. Selecting all 4096 and then
  // downsampling to a fixed point budget keeps every 13th row, which
  // throws away roughly seven eighths of the readings and leaves the
  // chart's tooltip empty at most cursor positions.
  const queries = useMemo(
    () => [
      ...driveStatTitles.map(catalogQuery),
      catalogQuery('Route'),
      `SELECT timestamp AS time,
        (CASE WHEN '$length_unit' = 'mi' THEN speed_kmh / 1.60934 ELSE speed_kmh END) AS "Speed ($length_unit/h)",
        power_kw AS "Power (kW)",
        (CASE WHEN '$length_unit' = 'mi' THEN NULLIF(range_km, 0) / 1.60934 ELSE NULLIF(range_km, 0) END) AS "Range (rated $length_unit)",
        (CASE WHEN '$length_unit' = 'mi' THEN NULLIF(est_range_km, 0) / 1.60934 ELSE NULLIF(est_range_km, 0) END) AS "Range (est. $length_unit)",
        battery_level AS "SOC (%)",
        NULLIF(usable_battery_level, 0) AS "Usable SOC (%)",
        battery_heater_on AS "Battery heater"
       FROM positions WHERE drive_id = $drive_id ORDER BY timestamp`,
      `SELECT timestamp AS time,
        (CASE WHEN '$length_unit' = 'mi' THEN elevation_m * 3.28084 ELSE elevation_m END) AS "Elevation"
       FROM positions WHERE drive_id = $drive_id AND elevation_m IS NOT NULL ORDER BY timestamp`,
      `SELECT timestamp AS time,
        (CASE WHEN '$temp_unit' = 'F' THEN outside_temp_c * 9.0 / 5 + 32 ELSE outside_temp_c END) AS "Outside (°$temp_unit)",
        (CASE WHEN '$temp_unit' = 'F' THEN inside_temp_c * 9.0 / 5 + 32 ELSE inside_temp_c END) AS "Inside (°$temp_unit)",
        (CASE WHEN '$temp_unit' = 'F' THEN NULLIF(driver_temp_setting_c, 0) * 9.0 / 5 + 32 ELSE NULLIF(driver_temp_setting_c, 0) END) AS "Driver (°$temp_unit)",
        (CASE WHEN '$temp_unit' = 'F' THEN NULLIF(passenger_temp_setting_c, 0) * 9.0 / 5 + 32 ELSE NULLIF(passenger_temp_setting_c, 0) END) AS "Passenger (°$temp_unit)",
        is_climate_on AS "Climate",
        fan_status AS "Fan"
       FROM positions WHERE drive_id = $drive_id
         AND (outside_temp_c IS NOT NULL OR inside_temp_c IS NOT NULL
              OR is_climate_on IS NOT NULL OR fan_status IS NOT NULL)
       ORDER BY timestamp`,
      `SELECT timestamp AS time,
        NULLIF(tpms_pressure_fl, 0) AS "Front left (bar)", NULLIF(tpms_pressure_fr, 0) AS "Front right (bar)",
        NULLIF(tpms_pressure_rl, 0) AS "Rear left (bar)", NULLIF(tpms_pressure_rr, 0) AS "Rear right (bar)"
       FROM positions WHERE drive_id = $drive_id
         AND COALESCE(NULLIF(tpms_pressure_fl, 0), NULLIF(tpms_pressure_fr, 0),
                      NULLIF(tpms_pressure_rl, 0), NULLIF(tpms_pressure_rr, 0)) IS NOT NULL
       ORDER BY timestamp`,
    ],
    [definition],
  )
  const [results, setResults] = useState<readonly QueryResult[]>([])
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    let active = true
    // The full settings, not a subset: the drive-detail stat panels come
    // from the shared catalog and use  / , so
    // omitting preferredRange silently pinned this view to rated range
    // however the Range type control was set.
    executeQueries(bytes, queries, {
      driveId,
      lengthUnit: settings.lengthUnit,
      temperatureUnit: settings.temperatureUnit,
      preferredRange: settings.preferredRange,
      minDistance: settings.minDistance,
      statisticsPeriod: settings.statisticsPeriod,
    })
      .then((value) => {
        if (active) setResults(value)
      })
      .catch((reason: unknown) => {
        if (active)
          setError(reason instanceof Error ? reason.message : 'Drive details query failed.')
      })
    return () => {
      active = false
    }
  }, [bytes, driveId, queries, settings])
  // Result indices are derived from the query list above rather than
  // written out, so adding a stat card cannot silently shift the charts.
  const routeIndex = driveStatTitles.length
  const telemetryIndex = routeIndex + 1
  const elevationIndex = routeIndex + 2
  const temperatureIndex = routeIndex + 3
  const tyreIndex = routeIndex + 4
  // route and its parsed timestamps are memoised because hoverTime
  // changes on every mouse move. Rebuilt inline, a 4096-position drive
  // reallocated the whole array and re-parsed every timestamp - roughly
  // twelve thousand Date constructions - for each pixel of cursor
  // movement, which is what made the crosshair feel like it was
  // stuttering rather than following.
  const route: readonly Point[] = useMemo(
    () =>
      (results[routeIndex]?.rows ?? []).flatMap<Point>((row) => {
        const latitude = number(row[0])
        const longitude = number(row[1])
        const speedKmh = number(row[2]) ?? undefined
        const timestamp = typeof row[3] === 'string' ? row[3] : undefined
        if (latitude === null || longitude === null) return []
        return [{ latitude, longitude, ...(speedKmh === undefined ? {} : { speedKmh }), ...(timestamp === undefined ? {} : { timestamp }) }]
      }),
    [results, routeIndex],
  )
  const routeEpochs = useMemo(
    () => route.map((point) => (point.timestamp === undefined ? Number.NaN : epochMs(point.timestamp))),
    [route],
  )
  const activePoint = useMemo(() => {
    if (hoverTime === null) return undefined
    const target = epochMs(hoverTime)
    if (Number.isNaN(target)) return undefined
    let closest: Point | undefined
    let closestGap = Number.POSITIVE_INFINITY
    for (const [index, point] of route.entries()) {
      const gap = Math.abs((routeEpochs[index] ?? Number.NaN) - target)
      if (gap < closestGap) {
        closestGap = gap
        closest = point
      }
    }
    return closest
  }, [hoverTime, route, routeEpochs])
  const exportGpx = (): void => {
    const points = route.map((point) => `<trkpt lat="${point.latitude}" lon="${point.longitude}">${point.timestamp ? `<time>${timestampDate(point.timestamp).toISOString()}</time>` : ''}</trkpt>`).join('')
    const gpx = `<?xml version="1.0" encoding="UTF-8"?><gpx version="1.1" creator="teslalog viewer" xmlns="http://www.topografix.com/GPX/1/1"><trk><name>Drive ${driveId}</name><trkseg>${points}</trkseg></trk></gpx>`
    const url = URL.createObjectURL(new Blob([gpx], { type: 'application/gpx+xml' }))
    const link = document.createElement('a')
    link.href = url
    link.download = `drive-${driveId}.gpx`
    link.click()
    URL.revokeObjectURL(url)
  }
  const panelTitle = (title: string): string =>
    title
      .replaceAll('$length_unit', settings.lengthUnit)
      .replaceAll('$temp_unit', settings.temperatureUnit)
  const statValue = (index: number, title: string): string => {
    const row = results[index]?.rows[0]
    if (!row || row[0] === null) return '—'
    // Battery is the one two-column stat: the catalog query returns start
    // and end so Grafana can render them side by side.
    if (title === 'Battery start → end') return `${text(row[0])}% → ${text(row[1])}%`
    return text(row[0])
  }
  return (
    <main>
      <button className="back-button" type="button" onClick={onBack}>
        ← All drives
      </button>
      <button className="gpx-button" type="button" onClick={exportGpx} disabled={route.length === 0}>Export GPX</button>
      <Heading title={`Drive ${driveId}`} note="Drive details" />
      {error && <p className="no-data">{error}</p>}
      <section className="cards">
        {driveStatTitles.map((title, index) => (
          <article key={title}>
            <span>{panelTitle(title)}</span>
            <strong>{statValue(index, title)}</strong>
          </article>
        ))}
      </section>
      <section className="drive-detail-grid">
        <TelemetryChart result={results[telemetryIndex]} title="Drive" onHoverTime={setHoverTime} />
        <article className="catalog-panel drive-route-panel">
          <h2>Route</h2>
          <RouteMap points={route} activePoint={activePoint} lengthUnit={settings.lengthUnit} />
        </article>
        <TelemetryChart
          result={results[elevationIndex]}
          title={`Elevation (${settings.lengthUnit === 'mi' ? 'ft' : 'm'})`}
          onHoverTime={setHoverTime}
        />
        <TelemetryChart
          result={results[temperatureIndex]}
          title="Temperatures"
          onHoverTime={setHoverTime}
        />
        <TelemetryChart
          result={results[tyreIndex]}
          title="Tire pressure"
          onHoverTime={setHoverTime}
        />
      </section>
    </main>
  )
}

export function ChargeDetailsDashboard({
  bytes,
  chargingSessionId,
  onBack,
  settings,
}: Readonly<{
  bytes: Uint8Array
  chargingSessionId: number
  onBack: () => void
  settings: ViewSettings
}>) {
  const queries = useMemo(() => [
    `SELECT COALESCE(location, printf('%.4f, %.4f',latitude,longitude)) location,
      ROUND((julianday(end_time)-julianday(start_time))*24*60,0) duration,
      start_battery_level, end_battery_level, charge_energy_added_kwh, charge_energy_used_kwh,
      ROUND(charge_energy_added_kwh*100.0/NULLIF(charge_energy_used_kwh,0),1) efficiency,
      cost, max_charger_power_kw, outside_temp_avg_c, start_range_km, end_range_km,
      latitude, longitude, CASE WHEN is_dc_fast_charge=1 THEN 'DC' ELSE 'AC' END type
     FROM charging_sessions WHERE id=$charging_session_id`,
    `SELECT timestamp time, battery_level "SOC (%)", charger_power_kw "Power (kW)",
      battery_heater_on "Battery heater",
      CASE WHEN '$length_unit'='mi' THEN range_km/1.60934 ELSE range_km END "Range ($length_unit)",
      charger_voltage "Charging voltage (V)", charger_phases "Phases",
      charger_actual_current "Current (A)", charger_pilot_current "Current pilot (A)",
      CASE WHEN '$temp_unit'='F' THEN outside_temp_c*9.0/5+32 ELSE outside_temp_c END "Outdoor (°$temp_unit)"
     FROM charging_samples WHERE charging_session_id=$charging_session_id ORDER BY timestamp`,
    `SELECT battery_level "SOC (%)", charger_power_kw "Power (kW)" FROM charging_samples
     WHERE charging_session_id=$charging_session_id AND battery_level IS NOT NULL ORDER BY timestamp`,
  ], [])
  const [results, setResults] = useState<readonly QueryResult[]>([])
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    let active = true
    executeQueries(bytes, queries, { chargingSessionId, lengthUnit: settings.lengthUnit, temperatureUnit: settings.temperatureUnit, preferredRange: settings.preferredRange, minDistance: settings.minDistance, statisticsPeriod: settings.statisticsPeriod })
      .then((value) => { if (active) setResults(value) })
      .catch((reason: unknown) => { if (active) setError(reason instanceof Error ? reason.message : 'Charge details query failed.') })
    return () => { active = false }
  }, [bytes, chargingSessionId, queries, settings])
  const row = results[0]?.rows[0]
  const n = (index: number): number | null => number(row?.[index])
  const display = (index: number, digits = 1): string => {
    const value = n(index)
    return value === null ? '—' : value.toFixed(digits)
  }
  const duration = n(1)
  const temperatureValue = n(9)
  const temperatureText = temperatureValue === null ? '—' : `${(settings.temperatureUnit === 'F' ? temperatureValue * 9 / 5 + 32 : temperatureValue).toFixed(1)} °${settings.temperatureUnit}`
  const rangeFactor = settings.lengthUnit === 'mi' ? 1 / 1.60934 : 1
  const point = n(12) === null || n(13) === null ? [] : [{ latitude: n(12) ?? 0, longitude: n(13) ?? 0 }]
  const powers = (results[1]?.rows ?? []).flatMap((sample) => typeof sample[2] === 'number' ? [sample[2]] : [])
  const averagePower = powers.length === 0 ? null : powers.reduce((sum, power) => sum + power, 0) / powers.length
  return (
    <main>
      <button className="back-button" type="button" onClick={onBack}>← All charges</button>
      <Heading title={`Charge ${chargingSessionId}`} note={`${text(row?.[14])} charge details`} />
      {error && <p className="no-data">{error}</p>}
      <section className="charge-detail-summary">
        <article><span>Cost</span><strong>{display(7, 2)}</strong></article>
        <article><span>Duration</span><strong>{duration === null ? '—' : `${Math.floor(duration / 60)}h ${Math.round(duration % 60)}m`}</strong></article>
        <article><span>Energy added / used</span><strong>{display(4, 2)} / {display(5, 2)} kWh</strong><small>{display(6, 1)}% efficiency</small></article>
        <article><span>Battery level</span><strong>{display(2, 0)}% → {display(3, 0)}%</strong></article>
        <article><span>Ø power</span><strong>{averagePower === null ? '—' : averagePower.toFixed(2)} kW</strong></article>
        <article><span>Ø outdoor temperature</span><strong>{temperatureText}</strong></article>
        <article><span>Range ({settings.lengthUnit})</span><strong>{n(10) === null ? '—' : (n(10)! * rangeFactor).toFixed(1)} → {n(11) === null ? '—' : (n(11)! * rangeFactor).toFixed(1)}</strong></article>
      </section>
      <section className="charge-detail-grid">
        <TelemetryChart result={results[1]} title="Charge details" emptyMessage="No charging telemetry recorded." />
        <article className="catalog-panel drive-route-panel"><h2>{text(row?.[0])}</h2><RouteMap points={point} /></article>
        <ChargingCurveChart result={results[2]} />
      </section>
    </main>
  )
}
