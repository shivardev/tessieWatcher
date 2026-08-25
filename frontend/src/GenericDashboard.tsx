import { useEffect, useMemo, useState, type CSSProperties } from 'react'
import {
  CartesianGrid,
  Bar,
  BarChart,
  Cell,
  Legend,
  Line,
  LineChart,
  Pie,
  PieChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { dashboardCatalog, type PanelDefinition } from './catalog'
import { nearestTimeSync } from './chartSync'
import { StateTimeline, spansFromRows } from './StateTimeline'
import { executeQueries, interpolateLabel } from './database'
import type { QueryResult, QueryValue } from './domain'
import { distance, speed, temperature, timestampDate, type ViewSettings } from './viewSettings'

type PanelState = Readonly<{ definition: PanelDefinition; results: readonly QueryResult[] }>
const seriesColors = ['#5794f2', '#ff9830', '#b877d9', '#73bf69', '#fade2a', '#8ab8ff', '#f2495c', '#56a64b'] as const
export const convertLabel = (label: string, settings: ViewSettings): string =>
  label
    .replaceAll('km/h', settings.lengthUnit === 'mi' ? 'mi/h' : 'km/h')
    .replaceAll('Wh/km', settings.lengthUnit === 'mi' ? 'Wh/mi' : 'Wh/km')
    .replaceAll('(km)', `(${settings.lengthUnit})`)
    // A bare "km" anywhere, not just after a space: "km lost / h" starts the
    // string, so the older ' km' rule left it reading km while the values
    // beside it had already been converted to miles.
    .replace(/\bkm\b/gu, settings.lengthUnit)
    .replaceAll('°C', `°${settings.temperatureUnit}`)

// Panels that alias their column `value` (every catalog stat panel does)
// carry the unit only in the panel title, so the title is the fallback
// conversion key for those columns.
//
// Deliberately restricted to placeholder column names rather than "any
// column with no unit word in it". The looser rule handed the panel
// title to every column of a table, so the Temperature - Driving
// Efficiency panel had its Efficiency column converted from Celsius: the
// word "Temperature" in the title matched, and 0.807 became 33.45.
const placeholderColumn = /^(?:value|count|total|amount|n)$/iu
export const unitKey = (column: string, panelTitle?: string): string =>
  panelTitle !== undefined && placeholderColumn.test(column.trim()) ? panelTitle : column

export const convertValue = (value: QueryValue, column: string, settings: ViewSettings): QueryValue => {
  if (typeof value !== 'number') return value
  if (settings.lengthUnit === 'mi') {
    // A column that already names the unit it is in was converted by its
    // own SQL (the catalog files double as real Grafana dashboards, where
    // there is no conversion layer, so some panels do it in the query).
    // Converting again would compound the error - and the heuristics
    // below match on words like "distance" and "range", which appear in
    // plenty of already-converted column names.
    if (/\b(?:mi|miles|mph)\b|Wh\/mi/iu.test(column)) return value
    if (/km\s*\/\s*h/iu.test(column)) return speed(value, settings.lengthUnit)
    // Distance divided by distance ("rated km lost per km driven") is the
    // same number in any unit; converting one side alone corrupts it.
    if (/\bper\s+km\b/iu.test(column)) return value
    // A per-distance rate (Wh/km, %/km) gets LARGER per mile, so it
    // multiplies where a plain distance divides.
    if (/\/\s*km\b/iu.test(column)) return value * 1.60934
    if (/(?:\(km\)|\bkm\b|range|distance|odometer)/iu.test(column)) {
      return distance(value, settings.lengthUnit)
    }
  }
  if (
    settings.temperatureUnit === 'F' &&
    // Same already-converted guard as for distance: a column that says °F
    // was converted by its own SQL, and running it through C->F again
    // turned 86.7 °F into 188.1.
    !/°F|fahrenheit/iu.test(column) &&
    /(?:°C|temperature|outside|inside|temp)/iu.test(column)
  ) {
    return temperature(value, settings.temperatureUnit)
  }
  return value
}

const transformResult = (
  result: QueryResult,
  settings: ViewSettings,
  panelTitle?: string,
): QueryResult => ({
  // error is carried through: it is the only signal the panel has that
  // its query failed rather than simply matching nothing, and dropping
  // it here turned a clear "this database has no address columns yet"
  // into a silent em-dash.
  ...(result.error === undefined ? {} : { error: result.error }),
  columns: result.columns.map((column) => convertLabel(column, settings)),
  rows: result.rows
    .filter((row) => {
      if (settings.timeRange === 'all' || !/^(?:time|date)$/iu.test(result.columns[0] ?? ''))
        return true
      if (typeof row[0] !== 'string') return true
      const rangeMilliseconds: Readonly<Record<Exclude<ViewSettings['timeRange'], 'all'>, number>> =
        {
          '24h': 24 * 60 * 60 * 1000,
          '7d': 7 * 24 * 60 * 60 * 1000,
          '30d': 30 * 24 * 60 * 60 * 1000,
          '90d': 90 * 24 * 60 * 60 * 1000,
          '1y': 365 * 24 * 60 * 60 * 1000,
        }
      return timestampDate(row[0]).getTime() >= Date.now() - rangeMilliseconds[settings.timeRange]
    })
    .map((row) =>
      row.map((value, index) =>
        convertValue(value, unitKey(result.columns[index] ?? '', panelTitle), settings),
      ),
    ),
})
const display = (value: QueryValue | undefined): string =>
  value === null || value === undefined
    ? '—'
    : value instanceof Uint8Array
      ? 'binary'
      : String(value)
const displayMetric = (value: QueryValue | undefined): string =>
  typeof value === 'number'
    ? new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 }).format(value)
    : display(value)

// isoTimestamp matches the RFC3339 form teslalog stores every time in
// (e.g. "2026-08-24T21:06:44.540879639Z"). Deliberately strict rather
// than handing every string to Date(): plenty of non-date strings parse
// into something, and silently reformatting a place name would be worse
// than leaving a timestamp raw.
const isoTimestamp = /^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}/

// displayCell formats a table cell for reading. Raw String() left
// full-precision floats ("111.78495532330024") and machine timestamps
// ("2026-08-24T21:06:44.540879639Z") on screen; stat panels already went
// through displayMetric, so tables were the odd one out.
const displayCell = (value: QueryValue | undefined): string => {
  if (typeof value === 'number') return displayMetric(value)
  if (typeof value === 'string' && isoTimestamp.test(value)) {
    const parsed = timestampDate(value)
    if (!Number.isNaN(parsed.getTime())) return parsed.toLocaleString()
  }
  return display(value)
}
const chartRows = (
  result: QueryResult,
): readonly Readonly<Record<string, string | number | null>>[] => {
  const sampleEvery = Math.max(1, Math.ceil(result.rows.length / 1_000))
  return result.rows
    .filter((_, index) => index % sampleEvery === 0 || index === result.rows.length - 1)
    .map((row) =>
      Object.fromEntries(
        result.columns.map((column, index) => [
          column,
          row[index] instanceof Uint8Array ? null : (row[index] ?? null),
        ]),
      ),
    )
}

const numericValues = (result: QueryResult, columnIndex: number): readonly number[] =>
  result.rows.flatMap((row) => (typeof row[columnIndex] === 'number' ? [row[columnIndex]] : []))

const seriesSummary = (result: QueryResult, columnIndex: number): string => {
  const values = numericValues(result, columnIndex)
  if (values.length === 0) return 'Mean: —  Max: —  Min: —'
  const mean = values.reduce((total, value) => total + value, 0) / values.length
  return `Mean: ${displayMetric(mean)}  Max: ${displayMetric(Math.max(...values))}  Min: ${displayMetric(Math.min(...values))}`
}

const timeTick = (value: string | number): string => {
  if (typeof value !== 'string') return String(value)
  const parsed = timestampDate(value)
  return Number.isNaN(parsed.getTime())
    ? value
    : parsed.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}

function StatPanel({ panel }: Readonly<{ panel: PanelState }>) {
  const value = panel.results[0]?.rows[0]?.[0]
  return (
    <article className="catalog-panel stat-panel">
      <h2>{panel.definition.title}</h2>
      <strong>{displayMetric(value)}</strong>
    </article>
  )
}

function TablePanel({ panel }: Readonly<{ panel: PanelState }>) {
  const result = panel.results[0]
  if (!result)
    return (
      <article className="catalog-panel">
        <h2>{panel.definition.title}</h2>
        <p className="no-data">No result.</p>
      </article>
    )
  return (
    <article className="catalog-panel table-panel">
      <h2>{panel.definition.title}</h2>
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
                <td key={`${rowIndex}-${result.columns[columnIndex] ?? columnIndex}`}>
                  {displayCell(value)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      {result.rows.length === 0 && <p className="no-data">No matching data.</p>}
    </article>
  )
}

function BarPanel({ panel }: Readonly<{ panel: PanelState }>) {
  const result = panel.results[0]
  const data = useMemo(() => (result ? chartRows(result) : []), [result])
  const category = result?.columns[0] ?? 'category'
  const series = result?.columns.slice(1) ?? []
  return (
    <article className="catalog-panel chart">
      <h2>{panel.definition.title}</h2>
      <ResponsiveContainer width="100%" height={260}>
        <BarChart data={[...data]} layout="vertical">
          <CartesianGrid stroke="#26322f" horizontal={false} />
          <XAxis type="number" stroke="#879491" />
          <YAxis dataKey={category} type="category" stroke="#879491" width={130} tick={{ fontSize: 11 }} />
          <Tooltip contentStyle={{ background: '#11191a', border: '1px solid #53605d', borderRadius: 4 }} />
          {series.map((key, index) => (
            <Bar key={key} dataKey={key} fill={seriesColors[index % seriesColors.length]} isAnimationActive={false} />
          ))}
        </BarChart>
      </ResponsiveContainer>
    </article>
  )
}

function TimeSeriesPanel({ panel }: Readonly<{ panel: PanelState }>) {
  const result = panel.results[0]
  const data = useMemo(() => (result ? chartRows(result) : []), [result])
  const xKey = result?.columns[0] ?? 'time'
  const series = result?.columns.slice(1) ?? []
  return (
    <article className="catalog-panel chart">
      <h2>{panel.definition.title}</h2>
      <ResponsiveContainer width="100%" height={260}>
        <LineChart data={[...data]} syncId="dashboard-time" syncMethod={nearestTimeSync}>
          <CartesianGrid stroke="#26322f" vertical={false} />
          <XAxis
            dataKey={xKey}
            stroke="#879491"
            minTickGap={45}
            tickFormatter={timeTick}
            tick={{ fontSize: 11 }}
          />
          <YAxis stroke="#879491" width={45} />
          <Tooltip
            contentStyle={{ background: '#11191a', border: '1px solid #53605d', borderRadius: 4 }}
            labelFormatter={(label) => timeTick(String(label))}
          />
          {series.map((key, index) => (
            <Line
              key={key}
              dataKey={key}
              dot={false}
              stroke={seriesColors[index % seriesColors.length] ?? '#5794f2'}
              connectNulls={false}
              isAnimationActive={false}
            />
          ))}
        </LineChart>
      </ResponsiveContainer>
      <div className="chart-series-summary">
        {series.map((key, index) => (
          <div key={key}>
            <span style={{ background: seriesColors[index % seriesColors.length] }} />
            <b>{key}</b>
            <small>{result ? seriesSummary(result, index + 1) : 'Mean: —  Max: —  Min: —'}</small>
          </div>
        ))}
      </div>
    </article>
  )
}

function PiePanel({ panel }: Readonly<{ panel: PanelState }>) {
  const result = panel.results[0]
  const data = (result?.rows ?? []).flatMap((row) =>
    typeof row[0] === 'string' && typeof row[1] === 'number'
      ? [{ name: row[0], value: row[1] }]
      : [],
  )
  const colors = ['#c9ff43', '#63d8ff', '#ff9e64', '#d5a6ff']
  return (
    <article className="catalog-panel chart">
      <h2>{panel.definition.title}</h2>
      <ResponsiveContainer width="100%" height={260}>
        <PieChart>
          <Pie data={data} dataKey="value" nameKey="name" innerRadius="48%" outerRadius="78%">
            {data.map((item, index) => (
              <Cell key={item.name} fill={colors[index % colors.length] ?? '#c9ff43'} />
            ))}
          </Pie>
          <Tooltip contentStyle={{ background: '#11191a', border: '1px solid #26322f' }} />
          <Legend />
        </PieChart>
      </ResponsiveContainer>
    </article>
  )
}

// The catalog's state-timeline panels return one row per state CHANGE,
// as a plain "time, state" series so the same SQL also works in Grafana.
// spansFromRows turns those points into intervals.
function StateTimelinePanel({ panel }: Readonly<{ panel: PanelState }>) {
  const result = panel.results[0]
  const spans = spansFromRows(result?.rows ?? [])
  return (
    <article className="catalog-panel timeline-panel">
      <StateTimeline spans={spans} title={panel.definition.title} />
    </article>
  )
}

function Panel({ panel }: Readonly<{ panel: PanelState }>) {
  // A panel whose own query failed says so where it sits, so the rest of
  // the dashboard still renders. "no such column" here means the database
  // predates the column - it fills in once the logger is updated.
  const failure = panel.results.find((result) => result.error !== undefined)?.error
  if (failure !== undefined)
    return (
      <article className="catalog-panel">
        <h2>{panel.definition.title}</h2>
        <p className="no-data">
          {/^no such column/iu.test(failure)
            ? `Not available in this database (${failure}). It will appear once teslalog has been updated and has re-resolved a location.`
            : failure}
        </p>
      </article>
    )
  if (panel.definition.type === 'stat' || panel.definition.type === 'gauge')
    return <StatPanel panel={panel} />
  if (panel.definition.type === 'table') return <TablePanel panel={panel} />
  if (panel.definition.type === 'barchart' || panel.definition.type === 'bargauge')
    return <BarPanel panel={panel} />
  if (panel.definition.type === 'piechart') return <PiePanel panel={panel} />
  if (panel.definition.type === 'state-timeline') return <StateTimelinePanel panel={panel} />
  return <TimeSeriesPanel panel={panel} />
}

const panelStyle = (panel: PanelDefinition): CSSProperties => ({
  gridColumn: `${panel.gridPos.x + 1} / span ${Math.max(1, panel.gridPos.w)}`,
  gridRow: `${panel.gridPos.y + 1} / span ${Math.max(1, panel.gridPos.h)}`,
})

export function GenericDashboard({
  catalogKey,
  databaseBytes,
  title,
  settings,
}: Readonly<{
  catalogKey: string
  databaseBytes: Uint8Array
  title?: string
  settings: ViewSettings
}>) {
  const definition = dashboardCatalog.find((dashboard) => dashboard.key === catalogKey)
  const [panels, setPanels] = useState<readonly PanelState[]>([])
  // Starts true, for the same reason as the custom dashboards: the first
  // render precedes any query, and showing that as an empty grid reads as
  // no data rather than as loading.
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    let active = true
    setLoading(true)
    if (!definition)
      return () => {
        active = false
      }
    const load = async (): Promise<void> => {
      try {
        const queries = definition.panels.flatMap((panel) => panel.queries)
        const variables = {
          minimumIdleHours: 1,
          lengthUnit: settings.lengthUnit,
          temperatureUnit: settings.temperatureUnit,
          timeRange: settings.timeRange,
          preferredRange: settings.preferredRange,
          minDistance: settings.minDistance,
          statisticsPeriod: settings.statisticsPeriod,
        }
        const results = await executeQueries(databaseBytes, queries, variables)
        let offset = 0
        const next = definition.panels.map((panel) => {
          // The title is interpolated before anything reads it, because
          // the unit conversion uses it as a fallback hint: a raw
          // "$preferred_range" contains the word "range" and would get
          // the panel's value converted as though it were a distance.
          const panelTitle = interpolateLabel(panel.title, variables)
          const panelResults = results
            .slice(offset, offset + panel.queries.length)
            .map((result) => transformResult(result, settings, panelTitle))
          offset += panel.queries.length
          return { definition: { ...panel, title: panelTitle }, results: panelResults }
        })
        if (active) setPanels(next)
      } catch (reason: unknown) {
        if (active) setError(reason instanceof Error ? reason.message : 'Dashboard query failed.')
      } finally {
        if (active) setLoading(false)
      }
    }
    void load()
    return () => {
      active = false
    }
  }, [databaseBytes, definition, settings])
  if (!definition)
    return (
      <main>
        <p className="no-data">Dashboard definition unavailable.</p>
      </main>
    )
  return (
    <main>
      <header className="page-head">
        <div>
          <span className="eyebrow">teslalog analytics</span>
          <h1>{title ?? definition.title.replace('teslalog: ', '')}</h1>
        </div>
      </header>
      {error && (
        <div className="error" role="alert">
          {error}
        </div>
      )}
      {loading && panels.length === 0 && (
        <p className="dashboard-loading" role="status" aria-live="polite">
          <span className="spinner" aria-hidden="true" />
          Reading the database…
        </p>
      )}
      <section className="catalog-grid grafana-grid">
        {panels.map((panel) => (
          <div className="grafana-grid-item" key={panel.definition.id} style={panelStyle(panel.definition)}>
            <Panel
              panel={{
                ...panel,
                definition: {
                  ...panel.definition,
                  title: convertLabel(panel.definition.title, settings),
                },
              }}
            />
          </div>
        ))}
      </section>
    </main>
  )
}
