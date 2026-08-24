import { lazy, Suspense, useRef, useState, type ChangeEvent, type ReactNode } from 'react'
import { CarFront, Database, Gauge, Menu, Upload, X } from 'lucide-react'
import { groups, type Dashboard, type DriveRow, type LoadedDatabase, type Metric } from './domain'
import { openDatabase, openDatabaseBytes } from './database'
import { importTeslaMateDump, isPostgresDump, type ImportProgress } from './teslamateImport'
import { catalogDashboardKeys } from './dashboardRegistry'
import {
  BatteryHealthDashboard,
  DatabaseInformationDashboard,
  DriveDetailsDashboard,
  ChargeDetailsDashboard,
  ChargingStatsDashboard,
  OverviewDashboard,
  ProjectedRangeDashboard,
  TripDashboard,
  VisitedDashboard,
} from './SpecialDashboards'
import './App.css'
import {
  defaultViewSettings,
  distance,
  speed,
  temperature,
  timestampDate,
  type LengthUnit,
  type TemperatureUnit,
  type TimeRange,
  type ViewSettings,
} from './viewSettings'

const GenericDashboard = lazy(async () => {
  const module = await import('./GenericDashboard')
  return { default: module.GenericDashboard }
})

const format = (value: number, digits = 0): string =>
  new Intl.NumberFormat(undefined, { maximumFractionDigits: digits }).format(value)
const cell = (value: number | null, digits = 0): string =>
  value === null ? '—' : format(value, digits)
const date = (value: string): string =>
  new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(
    timestampDate(value),
  )
const withinRange = (value: string, range: TimeRange): boolean => {
  if (range === 'all') return true
  const milliseconds: Readonly<Record<Exclude<TimeRange, 'all'>, number>> = {
    '24h': 24 * 60 * 60 * 1000,
    '7d': 7 * 24 * 60 * 60 * 1000,
    '30d': 30 * 24 * 60 * 60 * 1000,
    '90d': 90 * 24 * 60 * 60 * 1000,
    '1y': 365 * 24 * 60 * 60 * 1000,
  }
  return timestampDate(value).getTime() >= Date.now() - milliseconds[range]
}
const chargeDuration = (minutes: number | null): string => {
  if (minutes === null) return '—'
  if (minutes < 60) return `${minutes.toFixed(1)} min`
  const hours = minutes / 60
  return `${hours.toFixed(hours < 10 ? 1 : 1)} ${Math.abs(hours - 1) < 0.05 ? 'hour' : 'hours'}`
}

function Page({
  title,
  eyebrow,
  children,
}: Readonly<{ title: string; eyebrow: string; children: ReactNode }>) {
  return (
    <main>
      <header className="page-head">
        <div>
          <span className="eyebrow">{eyebrow}</span>
          <h1>{title}</h1>
        </div>
      </header>
      {children}
    </main>
  )
}

function MetricCards({ metrics }: Readonly<{ metrics: readonly Metric[] }>) {
  return (
    <section className="cards">
      {metrics.map((metric) => (
        <article key={metric.label}>
          <Gauge />
          <span>{metric.label}</span>
          <strong>{format(metric.value, 1)}</strong>
          <small>{metric.unit}</small>
        </article>
      ))}
    </section>
  )
}

function Drives({
  data,
  onSelect,
  settings,
}: Readonly<{
  data: LoadedDatabase
  onSelect: (driveId: number) => void
  settings: ViewSettings
}>) {
  type DriveSort = 'time' | 'from' | 'to' | 'durationMin' | 'distanceKm' | 'startBattery' | 'endBattery' | 'outsideTempC' | 'averageSpeedKmh' | 'maxSpeedKmh'
  const [geofence, setGeofence] = useState('All')
  const [location, setLocation] = useState('')
  const [minimumDistance, setMinimumDistance] = useState('0')
  const [minimumSpeed, setMinimumSpeed] = useState('0')
  const [efficiency, setEfficiency] = useState<'slope-adjusted' | 'by distance'>('slope-adjusted')
  const [sort, setSort] = useState<DriveSort>('time')
  const [descending, setDescending] = useState(true)
  const places = [...new Set(data.drives.flatMap((drive) => [drive.from, drive.to]))].sort()
  const convertedDistance = (drive: DriveRow): number => distance(drive.distanceKm ?? 0, settings.lengthUnit)
  const convertedSpeed = (value: number | null): number => speed(value ?? 0, settings.lengthUnit)
  const sortValue = (drive: DriveRow, key: DriveSort): string | number => {
    if (key === 'time') return timestampDate(drive.time).getTime()
    if (key === 'from' || key === 'to') return drive[key].toLocaleLowerCase()
    return drive[key] ?? Number.NEGATIVE_INFINITY
  }
  const toggleSort = (key: DriveSort): void => {
    if (sort === key) setDescending((value) => !value)
    else { setSort(key); setDescending(false) }
  }
  const drives = data.drives
    .filter((drive) => withinRange(drive.time, settings.timeRange))
    .filter((drive) => geofence === 'All' || drive.from === geofence || drive.to === geofence)
    .filter((drive) => `${drive.from} ${drive.to}`.toLocaleLowerCase().includes(location.trim().toLocaleLowerCase()))
    .filter((drive) => convertedDistance(drive) >= Number(minimumDistance || 0))
    .filter((drive) => convertedSpeed(drive.maxSpeedKmh) >= Number(minimumSpeed || 0))
    .toSorted((left, right) => {
      const a = sortValue(left, sort)
      const b = sortValue(right, sort)
      const comparison = typeof a === 'number' && typeof b === 'number' ? a - b : String(a).localeCompare(String(b))
      return descending ? -comparison : comparison
    })
  const totalDistance = drives.reduce((sum, drive) => sum + (drive.distanceKm ?? 0), 0)
  const totalDuration = drives.reduce((sum, drive) => sum + (drive.durationMin ?? 0), 0)
  const totalEnergy = drives.reduce((sum, drive) => sum + (drive.energyKwh ?? 0), 0)
  const consumption = totalDistance === 0 ? 0 : totalEnergy * 1000 / distance(totalDistance, settings.lengthUnit)
  const driveMetrics: readonly Metric[] = [
    { label: 'Total energy consumed (net)', value: totalEnergy, unit: 'kWh' },
    { label: 'Total duration', value: totalDuration / (60 * 24), unit: 'days' },
    {
      label: 'Total distance',
      value: distance(totalDistance, settings.lengthUnit),
      unit: settings.lengthUnit,
    },
    {
      label: 'Ø consumption (net)',
      value: consumption,
      unit: `Wh/${settings.lengthUnit}`,
    },
  ]
  return (
    <Page title="Drives" eyebrow={settings.timeRange === 'all' ? 'All time' : settings.timeRange}>
      <section className="charge-filters drive-filters" aria-label="Drive filters">
        <label>Geofence<select value={geofence} onChange={(event) => setGeofence(event.target.value)}><option>All</option>{places.map((place) => <option key={place}>{place}</option>)}</select></label>
        <label>Location<input value={location} onChange={(event) => setLocation(event.target.value)} placeholder="Enter value" /></label>
        <label>Distance ≥<input type="number" min="0" value={minimumDistance} onChange={(event) => setMinimumDistance(event.target.value)} /></label>
        <label>Speed ≥<input type="number" min="0" value={minimumSpeed} onChange={(event) => setMinimumSpeed(event.target.value)} /></label>
        <label title={efficiency === 'slope-adjusted' ? 'Accounts for ascent, descent, 85% regenerative braking efficiency, and a 2100 kg vehicle.' : 'Compares distance driven with rated range lost.'}>Efficiency<select value={efficiency} onChange={(event) => setEfficiency(event.target.value as 'slope-adjusted' | 'by distance')}><option value="slope-adjusted">slope-adjusted</option><option value="by distance">by distance</option></select></label>
      </section>
      <MetricCards metrics={driveMetrics} />
      <h2 className="table-heading">Drive</h2>
      <div className="table-panel drives-table">
        <table>
          <thead>
            <tr>
              {([['time','Date'],['from','Start'],['to','Destination'],['durationMin','Duration'],['distanceKm','Distance'],['startBattery','% Start'],['endBattery','% End'],['outsideTempC','Temp'],['averageSpeedKmh','Ø Speed'],['maxSpeedKmh','max Speed']] as const).map(([key,label]) => <th key={key}><button type="button" onClick={() => toggleSort(key)}>{label}{sort === key ? (descending ? ' ↓' : ' ↑') : ''}</button></th>)}
            </tr>
          </thead>
          <tbody>
            {drives.map((drive) => (
              <tr key={drive.id}>
                <td>
                  <button className="table-link" type="button" onClick={() => onSelect(drive.id)}>
                    {date(drive.time)}
                  </button>
                </td>
                <td title={drive.from}>{drive.from}</td>
                <td title={drive.to}>{drive.to}</td>
                <td>{chargeDuration(drive.durationMin)}</td>
                <td>
                  {drive.distanceKm === null
                    ? '—'
                    : cell(distance(drive.distanceKm, settings.lengthUnit), 1)}{' '}
                  {settings.lengthUnit}
                </td>
                <td>{cell(drive.startBattery)}%</td>
                <td>{cell(drive.endBattery)}%</td>
                <td>{drive.outsideTempC === null ? '—' : `${format(temperature(drive.outsideTempC, settings.temperatureUnit), 1)} °${settings.temperatureUnit}`}</td>
                <td>{drive.averageSpeedKmh === null ? '—' : `${cell(speed(drive.averageSpeedKmh, settings.lengthUnit))} ${settings.lengthUnit}/h`}</td>
                <td>
                  {drive.maxSpeedKmh === null
                    ? '—'
                    : cell(speed(drive.maxSpeedKmh, settings.lengthUnit))}{' '}
                  {settings.lengthUnit}/h
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {drives.length === 0 && <p className="no-data">No closed drives found.</p>}
      </div>
    </Page>
  )
}

function DriveStats({
  data,
  settings,
}: Readonly<{ data: LoadedDatabase; settings: ViewSettings }>) {
  const maxVisits = Math.max(1, ...data.destinations.map((item) => item.visits))
  return (
    <Page title="Drive stats" eyebrow="Lifetime">
      <MetricCards
        metrics={data.lifetimeDriveMetrics.map((metric) =>
          metric.unit === 'km'
            ? {
                ...metric,
                value: distance(metric.value, settings.lengthUnit),
                unit: settings.lengthUnit,
              }
            : metric.unit === 'km/h'
              ? {
                  ...metric,
                  value: speed(metric.value, settings.lengthUnit),
                  unit: `${settings.lengthUnit}/h`,
                }
              : metric,
        )}
      />
      <section className="chart-panel">
        <h2>Top 10 destinations</h2>
        {data.destinations.map((item) => (
          <div className="bar-row" key={item.name}>
            <span>{item.name}</span>
            <div>
              <i style={{ width: `${(item.visits / maxVisits) * 100}%` }} />
            </div>
            <b>{item.visits}</b>
          </div>
        ))}
        {data.destinations.length === 0 && <p className="no-data">No named destinations found.</p>}
      </section>
    </Page>
  )
}

function Charges({ data, settings, onSelect }: Readonly<{ data: LoadedDatabase; settings: ViewSettings; onSelect: (chargeId: number) => void }>) {
  const [type, setType] = useState<'All' | 'AC' | 'DC'>('All')
  const [geofence, setGeofence] = useState('All')
  const [location, setLocation] = useState('')
  const [minimumCost, setMinimumCost] = useState('')
  const [minimumDuration, setMinimumDuration] = useState('0')
  const geofences = [...new Set(data.charges.map((charge) => charge.location))].sort()
  const charges = data.charges.filter((charge) =>
    withinRange(charge.time, settings.timeRange) &&
    (geofence === 'All' || charge.location === geofence) &&
    (type === 'All' || charge.type === type) &&
    charge.location.toLocaleLowerCase().includes(location.trim().toLocaleLowerCase()) &&
    (minimumCost === '' || (charge.cost ?? 0) >= Number(minimumCost)) &&
    (minimumDuration === '' || (charge.durationMin ?? 0) >= Number(minimumDuration)),
  )
  const chargeMetrics: readonly Metric[] = [
    {
      label: 'Total energy added',
      value: charges.reduce((sum, charge) => sum + (charge.energyAddedKwh ?? 0), 0),
      unit: 'kWh',
    },
    { label: 'Total energy used', value: charges.reduce((sum, charge) => sum + (charge.energyUsedKwh ?? 0), 0), unit: 'kWh' },
    {
      label: 'Total charging cost',
      value: charges.reduce((sum, charge) => sum + (charge.cost ?? 0), 0),
      unit: 'cost',
    },
    {
      label: 'Average duration',
      value: charges.length === 0 ? 0 : charges.reduce((sum, charge) => sum + (charge.durationMin ?? 0), 0) / charges.length / 60,
      unit: 'hours',
    },
  ]
  return (
    <Page title="Charges" eyebrow={settings.timeRange === 'all' ? 'All time' : settings.timeRange}>
      <section className="charge-filters" aria-label="Charge filters">
        <label>Geofence<select value={geofence} onChange={(event) => setGeofence(event.target.value)}><option>All</option>{geofences.map((name) => <option key={name}>{name}</option>)}</select></label>
        <label>Location<input value={location} onChange={(event) => setLocation(event.target.value)} placeholder="Enter value" /></label>
        <label>Type<select value={type} onChange={(event) => setType(event.target.value as 'All' | 'AC' | 'DC')}><option>All</option><option>AC</option><option>DC</option></select></label>
        <label>Cost ≥<input type="number" min="0" step="0.01" value={minimumCost} onChange={(event) => setMinimumCost(event.target.value)} placeholder="Enter value" /></label>
        <label>Duration (minutes) ≥<input type="number" min="0" value={minimumDuration} onChange={(event) => setMinimumDuration(event.target.value)} /></label>
      </section>
      <MetricCards metrics={chargeMetrics} />
      <h2 className="table-heading">Charger type: {type}</h2>
      <div className="table-panel charges-table">
        <table>
          <thead>
            <tr>
              <th>When</th>
              <th>Location</th>
              <th>Type</th>
              <th>Duration</th>
              <th>Cost</th>
              <th>Cost / kWh</th>
              <th>Energy added</th>
              <th>Energy used</th>
              <th>Efficiency</th>
              <th>Temp</th>
            </tr>
          </thead>
          <tbody>
            {charges.map((charge) => (
              <tr key={charge.id}>
                <td><button className="table-link" type="button" onClick={() => onSelect(charge.id)}>{date(charge.time)}</button></td>
                <td title={charge.location}>{charge.location}</td>
                <td><span className={`charge-type ${charge.type.toLocaleLowerCase()}`}>{charge.type}</span></td>
                <td>{chargeDuration(charge.durationMin)}</td>
                <td>{cell(charge.cost, 2)}</td>
                <td>{cell(charge.cost === null || charge.energyUsedKwh === null || charge.energyUsedKwh === 0 ? null : charge.cost / charge.energyUsedKwh, 2)}</td>
                <td>{cell(charge.energyAddedKwh, 2)} kWh</td>
                <td>{cell(charge.energyUsedKwh, 2)} kWh</td>
                <td><span className="efficiency"><i style={{ width: `${Math.min(100, charge.efficiencyPercent ?? 0)}%` }} /> <b>{cell(charge.efficiencyPercent)}%</b></span></td>
                <td>{charge.outsideTempC === null ? '—' : `${format(temperature(charge.outsideTempC, settings.temperatureUnit), 1)} °${settings.temperatureUnit}`}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {charges.length === 0 && <p className="no-data">No closed charging sessions found.</p>}
      </div>
    </Page>
  )
}

function DashboardContent({
  active,
  data,
  selectedDriveId,
  selectedChargeId,
  onSelectDrive,
  onCloseDrive,
  onSelectCharge,
  onCloseCharge,
  settings,
}: Readonly<{
  active: Dashboard
  data: LoadedDatabase
  selectedDriveId: number | null
  selectedChargeId: number | null
  onSelectDrive: (driveId: number) => void
  onCloseDrive: () => void
  onSelectCharge: (chargeId: number) => void
  onCloseCharge: () => void
  settings: ViewSettings
}>) {
  if (selectedDriveId !== null)
    return (
      <DriveDetailsDashboard
        bytes={data.databaseBytes}
        driveId={selectedDriveId}
        onBack={onCloseDrive}
        settings={settings}
      />
    )
  if (selectedChargeId !== null)
    return <ChargeDetailsDashboard bytes={data.databaseBytes} chargingSessionId={selectedChargeId} onBack={onCloseCharge} settings={settings} />
  if (active === 'Overview') return <OverviewDashboard bytes={data.databaseBytes} settings={settings} />
  if (active === 'Drives')
    return <Drives data={data} onSelect={onSelectDrive} settings={settings} />
  if (active === 'Drive stats') return <DriveStats data={data} settings={settings} />
  if (active === 'Charges') return <Charges data={data} settings={settings} onSelect={onSelectCharge} />
  if (active === 'Charging stats') return <ChargingStatsDashboard bytes={data.databaseBytes} settings={settings} />
  if (active === 'Battery health')
    return <BatteryHealthDashboard bytes={data.databaseBytes} settings={settings} />
  if (active === 'Projected range')
    return <ProjectedRangeDashboard bytes={data.databaseBytes} settings={settings} />
  if (active === 'Trip') return <TripDashboard bytes={data.databaseBytes} settings={settings} onSelectDrive={onSelectDrive} />
  if (active === 'Visited')
    return <VisitedDashboard bytes={data.databaseBytes} settings={settings} />
  if (active === 'Database information')
    return <DatabaseInformationDashboard bytes={data.databaseBytes} />
  const catalogKey = catalogDashboardKeys[active]
  if (catalogKey)
    return (
      <Suspense
        fallback={
          <main>
            <p className="no-data">Loading dashboard…</p>
          </main>
        }
      >
        <GenericDashboard
          catalogKey={catalogKey}
          databaseBytes={data.databaseBytes}
          title={active}
          settings={settings}
        />
      </Suspense>
    )
  throw new Error(`Dashboard implementation registry is incomplete: ${active}`)
}

export default function App() {
  const input = useRef<HTMLInputElement>(null)
  const [data, setData] = useState<LoadedDatabase | null>(null)
  const [active, setActive] = useState<Dashboard>('Overview')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [importProgress, setImportProgress] = useState<ImportProgress | null>(null)
  const [menu, setMenu] = useState(false)
  const [selectedDriveId, setSelectedDriveId] = useState<number | null>(null)
  const [selectedChargeId, setSelectedChargeId] = useState<number | null>(null)
  const [settings, setSettings] = useState<ViewSettings>(defaultViewSettings)
  const choose = (): void => input.current?.click()
  const change = async (event: ChangeEvent<HTMLInputElement>): Promise<void> => {
    const file = event.target.files?.[0]
    event.target.value = ''
    if (!file) return
    setBusy(true)
    setImportProgress(null)
    setError(null)
    try {
      if (await isPostgresDump(file)) {
        const databaseBytes = await importTeslaMateDump(file, setImportProgress)
        const outputName = file.name.replace(/\.(?:dump|sql)$/iu, '') + '.db'
        setData(await openDatabaseBytes(outputName, databaseBytes.byteLength, databaseBytes))
      } else {
        setData(await openDatabase(file))
      }
      setActive('Overview')
      setSelectedDriveId(null)
      setSelectedChargeId(null)
    } catch (reason: unknown) {
      setError(reason instanceof Error ? reason.message : 'Could not open database.')
    } finally {
      setBusy(false)
      setImportProgress(null)
    }
  }
  return (
    <div className="shell">
      <input
        ref={input}
        hidden
        type="file"
        accept=".db,.sqlite,.sqlite3,.sql,.dump,application/sql"
        onChange={(event) => void change(event)}
      />
      <header className="top">
        <button
          className="menu"
          type="button"
          onClick={() => setMenu((value) => !value)}
          aria-label="Toggle navigation"
        >
          {menu ? <X /> : <Menu />}
        </button>
        <button
          className="brand"
          type="button"
          onClick={() => {
            setActive('Overview')
            setSelectedDriveId(null)
            setSelectedChargeId(null)
          }}
        >
          <i>T</i>
          <b>teslalog</b>
          <small>viewer</small>
        </button>
        <div className="top-actions">
          {data && (
            <div className="view-controls" aria-label="Dashboard display settings">
              <label>
                Range
                <select
                  aria-label="Time range"
                  value={settings.timeRange}
                  onChange={(event) =>
                    setSettings((current) => ({
                      ...current,
                      timeRange: event.target.value as TimeRange,
                    }))
                  }
                >
                  <option value="24h">24 hours</option>
                  <option value="7d">7 days</option>
                  <option value="30d">30 days</option>
                  <option value="90d">90 days</option>
                  <option value="1y">1 year</option>
                  <option value="all">All</option>
                </select>
              </label>
              <label>
                Distance
                <select
                  aria-label="Distance unit"
                  value={settings.lengthUnit}
                  onChange={(event) =>
                    setSettings((current) => ({
                      ...current,
                      lengthUnit: event.target.value as LengthUnit,
                    }))
                  }
                >
                  <option value="mi">Miles</option>
                  <option value="km">Kilometers</option>
                </select>
              </label>
              <label>
                Temp
                <select
                  aria-label="Temperature unit"
                  value={settings.temperatureUnit}
                  onChange={(event) =>
                    setSettings((current) => ({
                      ...current,
                      temperatureUnit: event.target.value as TemperatureUnit,
                    }))
                  }
                >
                  <option value="F">°F</option>
                  <option value="C">°C</option>
                </select>
              </label>
            </div>
          )}
          <button className="open" type="button" onClick={choose} disabled={busy}>
            <Upload />
            {busy ? 'Opening…' : data ? 'Change database' : 'Open database'}
          </button>
        </div>
      </header>
      {data && (
        <aside className={menu ? 'show' : ''}>
          {groups.map((group) => (
            <nav key={group.label}>
              <label>{group.label}</label>
              {group.items.map((item) => (
                <button
                  type="button"
                  className={active === item && selectedDriveId === null && selectedChargeId === null ? 'active' : ''}
                  onClick={() => {
                    setActive(item)
                    setSelectedDriveId(null)
                    setSelectedChargeId(null)
                    setMenu(false)
                  }}
                  key={item}
                >
                  {item}
                </button>
              ))}
            </nav>
          ))}
        </aside>
      )}
      {error && (
        <div className="error" role="alert">
          <b>Database could not be opened.</b> {error}
          <button type="button" onClick={() => setError(null)} aria-label="Dismiss">
            <X />
          </button>
        </div>
      )}
      {busy && importProgress && (
        <div className="import-progress" role="status" aria-live="polite">
          <div>
            <Database />
            <span>
              <b>Converting TeslaMate dump locally</b>
              <small>
                {importProgress.table
                  ? `Importing ${importProgress.table}`
                  : 'Preparing dashboards'}
                {' · '}
                {new Intl.NumberFormat().format(importProgress.rowsImported)} rows
              </small>
            </span>
          </div>
          <progress value={importProgress.bytesRead} max={importProgress.fileSize} />
          <strong>
            {Math.min(
              100,
              Math.round((importProgress.bytesRead / Math.max(importProgress.fileSize, 1)) * 100),
            )}
            %
          </strong>
        </div>
      )}
      {data ? (
        <DashboardContent
          active={active}
          data={data}
          selectedDriveId={selectedDriveId}
          selectedChargeId={selectedChargeId}
          onSelectDrive={setSelectedDriveId}
          onCloseDrive={() => setSelectedDriveId(null)}
          onSelectCharge={setSelectedChargeId}
          onCloseCharge={() => setSelectedChargeId(null)}
          settings={settings}
        />
      ) : (
        <main className="empty">
          <div>
            <span className="eyebrow">Private by design</span>
            <h1>
              Your Tesla history.<em>Clear at last.</em>
            </h1>
            <p>
              Open a teslalog SQLite database or a TeslaMate PostgreSQL plain dump and explore every
              drive, charge, route, and battery trend. Processing stays entirely in this browser.
            </p>
            <button className="cta" type="button" onClick={choose}>
              <Upload />
              Open database or dump
            </button>
            <small>
              <Database />
              Your file never leaves this device
            </small>
          </div>
          <section>
            <CarFront />
            <b>
              19 dashboards.
              <br />
              One private viewer.
            </b>
          </section>
        </main>
      )}
    </div>
  )
}
