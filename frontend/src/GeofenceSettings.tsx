import { useEffect, useState, type FormEvent } from 'react'
import { MapPin, Pencil, Plus, Trash2 } from 'lucide-react'
import { getRemoteBackend, runRemoteQuery, runRemoteStatement } from './remoteBackend'

type Billing = 'per_kwh' | 'per_minute'

type Geofence = Readonly<{
  id: number
  name: string
  latitude: number
  longitude: number
  radiusM: number
  billingType: Billing
  costPerUnit: number | null
  sessionFee: number
}>

type Usage = Readonly<{ charges: number; cost: number | null }>

type Pricing = 'none' | Billing
const empty = { name: '', latitude: '', longitude: '', radiusM: '50', pricing: 'none' as Pricing, costPerUnit: '', sessionFee: '' }
type Form = typeof empty

const sqlString = (value: string): string => `'${value.replaceAll("'", "''")}'`

// Prices are stored as REAL, so 0.13 reads back as 0.12999999523162842.
// Round for display and for the edit form; four decimals covers any real
// tariff ($0.1234/kWh) without showing float noise.
const tidy = (value: number, digits: number): number => Number(value.toFixed(digits))
const price = new Intl.NumberFormat('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 4 })
const dollars = (value: number): string => `$${price.format(value)}`
const coordinate = (value: number): string => value.toFixed(5)

const describePrice = (item: Geofence): string => {
  if (item.costPerUnit === null) return 'No price'
  if (item.costPerUnit === 0 && item.sessionFee === 0) return 'Free'
  const unit = `${dollars(item.costPerUnit)} / ${item.billingType === 'per_kwh' ? 'kWh' : 'min'}`
  return item.sessionFee > 0 ? `${unit} + ${dollars(item.sessionFee)} fee` : unit
}

// Accepts "35.10823, -85.06322" as copied from a map app.
const splitCoordinates = (value: string): readonly [string, string] | null => {
  const match = /^\s*(-?\d+(?:\.\d+)?)\s*[, ]\s*(-?\d+(?:\.\d+)?)\s*$/u.exec(value)
  return match ? [match[1] ?? '', match[2] ?? ''] : null
}

export function GeofenceSettings() {
  const backend = getRemoteBackend()
  const [items, setItems] = useState<Geofence[]>([])
  const [usage, setUsage] = useState<ReadonlyMap<number, Usage>>(new Map())
  const [editing, setEditing] = useState<number | null>(null)
  const [form, setForm] = useState<Form>(empty)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [applyHistory, setApplyHistory] = useState(true)
  const [notice, setNotice] = useState<string | null>(null)

  const load = async (): Promise<void> => {
    if (!backend) return
    const result = await runRemoteQuery(backend, `SELECT id, name, latitude, longitude, radius_m,
      billing_type, cost_per_unit, session_fee FROM geofences ORDER BY name`)
    if (result.error) throw new Error(result.error)
    const at = (row: readonly unknown[], name: string): unknown => row[result.columns.indexOf(name)]
    setItems(result.rows.map((row) => ({
      id: Number(at(row, 'id')),
      name: String(at(row, 'name')),
      latitude: Number(at(row, 'latitude')),
      longitude: Number(at(row, 'longitude')),
      radiusM: Number(at(row, 'radius_m')),
      billingType: String(at(row, 'billing_type')) === 'per_minute' ? 'per_minute' : 'per_kwh',
      costPerUnit: at(row, 'cost_per_unit') === null ? null : Number(at(row, 'cost_per_unit')),
      sessionFee: Number(at(row, 'session_fee') ?? 0),
    })))
    // How many closed charges each geofence covers, and what they cost:
    // the quickest check that a rule matches where you actually charge.
    // Counted by position (the same circle test the save uses), not by
    // charging_sessions.geofence_id: on the server that link can be empty
    // even when the charge sits inside the place (found October 2026 - 88
    // charges at home carried the place name with no geofence_id).
    const counts = await runRemoteQuery(backend, `SELECT g.id, COUNT(c.id) AS charges, SUM(c.cost) AS cost
      FROM geofences g JOIN charging_sessions c ON c.status='closed'
        AND c.latitude IS NOT NULL AND c.longitude IS NOT NULL
        AND 111320.0*SQRT((c.latitude-g.latitude)*(c.latitude-g.latitude)+
          (COS(g.latitude*0.0174532925199433)*(c.longitude-g.longitude))*(COS(g.latitude*0.0174532925199433)*(c.longitude-g.longitude)))<=g.radius_m
      GROUP BY g.id`)
    if (!counts.error)
      setUsage(new Map(counts.rows.map((row) => [Number(row[0]), { charges: Number(row[1]), cost: row[2] === null ? null : Number(row[2]) }])))
  }

  useEffect(() => { void load().catch((reason: unknown) => setError(reason instanceof Error ? reason.message : 'Could not load geofences.')) }, [])

  if (!backend)
    return (
      <main>
        <header className="page-head"><div><span className="eyebrow">System</span><h1>Geofences &amp; pricing</h1></div></header>
        <p className="no-data">Connect to the teslalog data server to edit geofences and charging prices.</p>
      </main>
    )

  const priced = form.pricing !== 'none'
  const billingType: Billing = form.pricing === 'per_minute' ? 'per_minute' : 'per_kwh'

  const save = async (event: FormEvent): Promise<void> => {
    event.preventDefault()
    setBusy(true); setError(null); setNotice(null)
    try {
      const lat = Number(form.latitude), lng = Number(form.longitude), radius = Number(form.radiusM)
      const unit = Number(form.costPerUnit || 0), fee = Number(form.sessionFee || 0)
      if (!form.name.trim()) throw new Error('Give the place a name.')
      if (form.latitude === '' || !Number.isFinite(lat) || lat < -90 || lat > 90 || form.longitude === '' || !Number.isFinite(lng) || lng < -180 || lng > 180)
        throw new Error('Enter valid coordinates (latitude −90 to 90, longitude −180 to 180).')
      if (!Number.isFinite(radius) || radius <= 0) throw new Error('The radius must be greater than zero.')
      if (priced && (!Number.isFinite(unit) || unit < 0 || !Number.isFinite(fee) || fee < 0))
        throw new Error('Charging prices cannot be negative.')
      const values = `${sqlString(form.name.trim())},${lat},${lng},${radius},${sqlString(billingType)},${priced ? unit : 'NULL'},${priced ? fee : 0}`
      const statement = editing === null
        ? `INSERT INTO geofences (name,latitude,longitude,radius_m,billing_type,cost_per_unit,session_fee) VALUES (${values})`
        : `UPDATE geofences SET (name,latitude,longitude,radius_m,billing_type,cost_per_unit,session_fee)=(${values}) WHERE id=${editing}`
      await runRemoteStatement(backend, statement)
      if (editing !== null) {
        await runRemoteStatement(backend, `UPDATE charging_sessions SET geofence_id=NULL WHERE geofence_id=${editing}`)
      }
      const saved = await runRemoteQuery(backend, `SELECT id FROM geofences WHERE name=${sqlString(form.name.trim())} LIMIT 1`)
      if (saved.error || saved.rows.length === 0) throw new Error(saved.error ?? 'Saved geofence could not be loaded.')
      const geofenceID = Number(saved.rows[0]?.[0])
      await runRemoteStatement(backend, `UPDATE charging_sessions SET geofence_id=${geofenceID},location=${sqlString(form.name.trim())}
        WHERE latitude IS NOT NULL AND longitude IS NOT NULL AND
        111320.0*SQRT((latitude-${lat})*(latitude-${lat})+
        (COS(${lat}*0.0174532925199433)*(longitude-${lng}))*(COS(${lat}*0.0174532925199433)*(longitude-${lng})))<=${radius}`)
      let pricedCount = 0
      if (applyHistory && priced) {
        const missing = await runRemoteQuery(backend, `SELECT COUNT(*) FROM charging_sessions
          WHERE geofence_id=${geofenceID} AND status='closed' AND cost IS NULL`)
        if (missing.error) throw new Error(missing.error)
        pricedCount = Number(missing.rows[0]?.[0] ?? 0)
        await runRemoteStatement(backend, `UPDATE charging_sessions SET cost=CASE
          WHEN ${sqlString(billingType)}='per_minute' THEN ${fee}+${unit}*((julianday(end_time)-julianday(start_time))*1440.0)
          ELSE ${fee}+${unit}*(CASE
            WHEN charge_energy_used_kwh IS NULL THEN COALESCE(charge_energy_added_kwh,0)
            WHEN charge_energy_added_kwh IS NULL THEN charge_energy_used_kwh
            WHEN charge_energy_used_kwh>charge_energy_added_kwh THEN charge_energy_used_kwh
            ELSE charge_energy_added_kwh END) END
          WHERE geofence_id=${geofenceID} AND status='closed' AND cost IS NULL`)
      }
      const name = form.name.trim()
      setEditing(null); setForm(empty); await load()
      setNotice(pricedCount > 0
        ? `Saved “${name}”. Priced ${pricedCount} earlier charge${pricedCount === 1 ? '' : 's'} that had no cost.`
        : `Saved “${name}”.`)
    } catch (reason: unknown) { setError(reason instanceof Error ? reason.message : 'Could not save geofence.') }
    finally { setBusy(false) }
  }

  const edit = (item: Geofence): void => {
    setEditing(item.id)
    setError(null); setNotice(null)
    setForm({
      name: item.name,
      latitude: String(tidy(item.latitude, 6)),
      longitude: String(tidy(item.longitude, 6)),
      radiusM: String(tidy(item.radiusM, 1)),
      pricing: item.costPerUnit === null ? 'none' : item.billingType,
      costPerUnit: item.costPerUnit === null ? '' : String(tidy(item.costPerUnit, 4)),
      sessionFee: item.sessionFee ? String(tidy(item.sessionFee, 2)) : '',
    })
  }
  const remove = async (item: Geofence): Promise<void> => {
    if (!globalThis.confirm(`Delete geofence “${item.name}”? Charges inside it keep their recorded cost.`)) return
    setBusy(true); setError(null); setNotice(null)
    try { await runRemoteStatement(backend, `DELETE FROM geofences WHERE id=${item.id}`); await load(); setNotice(`Deleted “${item.name}”.`) }
    catch (reason: unknown) { setError(reason instanceof Error ? reason.message : 'Could not delete geofence.') }
    finally { setBusy(false) }
  }

  const set = (key: keyof Form, value: string): void => setForm((current) => ({ ...current, [key]: value }))
  const setLatitude = (value: string): void => {
    const both = splitCoordinates(value)
    if (both) setForm((current) => ({ ...current, latitude: both[0], longitude: both[1] }))
    else set('latitude', value)
  }

  // Live example so the rate can be sanity-checked before saving.
  const unitValue = Number(form.costPerUnit || 0)
  const feeValue = Number(form.sessionFee || 0)
  const example = form.pricing === 'per_kwh'
    ? `A 30 kWh charge here costs ${dollars(feeValue + unitValue * 30)}.`
    : form.pricing === 'per_minute'
      ? `A 60-minute charge here costs ${dollars(feeValue + unitValue * 60)}.`
      : 'Charges here keep whatever cost they already have.'

  return (
    <main className="geofence-page">
      <header className="page-head">
        <div>
          <span className="eyebrow">System</span>
          <h1>Geofences &amp; pricing</h1>
          <p>Name the places you charge and what they cost. Saved to the server; the Pi picks changes up on its next sync.</p>
        </div>
      </header>
      {error && <p className="gf-message error" role="alert">{error}</p>}
      {notice && <p className="gf-message notice" role="status">{notice}</p>}

      <div className="gf-layout">
        <form className="gf-form" onSubmit={(event) => void save(event)}>
          <h2>{editing === null ? 'Add a place' : `Edit “${items.find((item) => item.id === editing)?.name ?? ''}”`}</h2>

          <fieldset>
            <legend>Place</legend>
            <label className="gf-wide">Name<input value={form.name} onChange={(event) => set('name', event.target.value)} placeholder="Home" /></label>
            <label>Latitude<input inputMode="decimal" value={form.latitude} onChange={(event) => setLatitude(event.target.value)} placeholder="35.10823" /></label>
            <label>Longitude<input inputMode="decimal" value={form.longitude} onChange={(event) => set('longitude', event.target.value)} placeholder="-85.06322" /></label>
            <label>Radius<span className="gf-affix"><input inputMode="decimal" value={form.radiusM} onChange={(event) => set('radiusM', event.target.value)} /><em>m</em></span></label>
            <small className="gf-wide">Tip: paste “latitude, longitude” from a map app into Latitude to fill both.</small>
          </fieldset>

          <fieldset>
            <legend>Charging price</legend>
            <div className="gf-segments gf-wide" role="radiogroup" aria-label="Charging price">
              {([['none', 'No price'], ['per_kwh', 'Per kWh'], ['per_minute', 'Per minute']] as const).map(([value, label]) => (
                <button key={value} type="button" role="radio" aria-checked={form.pricing === value} className={form.pricing === value ? 'active' : ''} onClick={() => setForm((current) => ({ ...current, pricing: value }))}>{label}</button>
              ))}
            </div>
            {priced && (
              <>
                <label>{form.pricing === 'per_kwh' ? 'Price per kWh' : 'Price per minute'}<span className="gf-affix prefix"><em>$</em><input inputMode="decimal" value={form.costPerUnit} onChange={(event) => set('costPerUnit', event.target.value)} placeholder="0.13" /></span></label>
                <label>Session fee<span className="gf-affix prefix"><em>$</em><input inputMode="decimal" value={form.sessionFee} onChange={(event) => set('sessionFee', event.target.value)} placeholder="0.00" /></span></label>
                <label className="gf-check gf-wide"><input type="checkbox" checked={applyHistory} onChange={(event) => setApplyHistory(event.target.checked)} /> Also price earlier charges here that have no cost yet</label>
              </>
            )}
            <small className="gf-wide">{example}</small>
          </fieldset>

          <div className="gf-actions">
            {editing !== null && <button type="button" className="gf-button" onClick={() => { setEditing(null); setForm(empty) }}>Cancel</button>}
            <button type="submit" className="gf-button primary" disabled={busy}>
              {editing === null ? <><Plus aria-hidden="true" /> Add place</> : 'Save changes'}
            </button>
          </div>
        </form>

        <section className="gf-list" aria-label="Saved places">
          <h2>{items.length} saved place{items.length === 1 ? '' : 's'}</h2>
          {items.length === 0 && <p className="no-data">No places yet. Add home, work or any charger you use.</p>}
          {items.map((item) => {
            const used = usage.get(item.id)
            return (
              <article key={item.id} className={`gf-item${editing === item.id ? ' editing' : ''}`}>
                <div className="gf-item-main">
                  <b>{item.name}</b>
                  <a href={`https://www.openstreetmap.org/?mlat=${item.latitude}&mlon=${item.longitude}#map=17/${item.latitude}/${item.longitude}`} target="_blank" rel="noreferrer">
                    <MapPin aria-hidden="true" /> {coordinate(item.latitude)}, {coordinate(item.longitude)} · {tidy(item.radiusM, 1)} m
                  </a>
                </div>
                <div className="gf-item-price">
                  <span className={item.costPerUnit === null ? 'muted' : ''}>{describePrice(item)}</span>
                  <small>{used ? `${used.charges} charge${used.charges === 1 ? '' : 's'}${used.cost !== null ? ` · ${dollars(tidy(used.cost, 2))}` : ''}` : 'No charges yet'}</small>
                </div>
                <div className="gf-item-actions">
                  <button type="button" className="gf-icon" aria-label={`Edit ${item.name}`} title="Edit" onClick={() => edit(item)}><Pencil aria-hidden="true" /></button>
                  <button type="button" className="gf-icon danger" aria-label={`Delete ${item.name}`} title="Delete" disabled={busy} onClick={() => void remove(item)}><Trash2 aria-hidden="true" /></button>
                </div>
              </article>
            )
          })}
        </section>
      </div>
    </main>
  )
}
