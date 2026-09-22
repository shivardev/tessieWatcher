import { useEffect, useState, type FormEvent } from 'react'
import { getRemoteBackend, runRemoteQuery, runRemoteStatement } from './remoteBackend'

type Geofence = Readonly<{
  id: number
  name: string
  latitude: number
  longitude: number
  radiusM: number
  billingType: 'per_kwh' | 'per_minute'
  costPerUnit: number | null
  sessionFee: number
}>

const empty = { name: '', latitude: '', longitude: '', radiusM: '50', billingType: 'per_kwh', priced: false, costPerUnit: '0', sessionFee: '0' }
type Form = typeof empty

const sqlString = (value: string): string => {
  const parts = value.replaceAll("'", "''").split(';').map((part) => `'${part}'`)
  return parts.join('||char(59)||')
}

export function GeofenceSettings() {
  const backend = getRemoteBackend()
  const [items, setItems] = useState<Geofence[]>([])
  const [editing, setEditing] = useState<number | null>(null)
  const [form, setForm] = useState<Form>(empty)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

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
      sessionFee: Number(at(row, 'session_fee')),
    })))
  }

  useEffect(() => { void load().catch((reason: unknown) => setError(reason instanceof Error ? reason.message : 'Could not load geofences.')) }, [])

  if (!backend) return <main><p className="no-data">Connect to Layerbase to modify geofences and charging prices.</p></main>

  const save = async (event: FormEvent): Promise<void> => {
    event.preventDefault()
    setBusy(true); setError(null)
    try {
      const lat = Number(form.latitude), lng = Number(form.longitude), radius = Number(form.radiusM)
      const unit = Number(form.costPerUnit), fee = Number(form.sessionFee)
      if (!form.name.trim() || !Number.isFinite(lat) || lat < -90 || lat > 90 || !Number.isFinite(lng) || lng < -180 || lng > 180 || !Number.isFinite(radius) || radius <= 0)
        throw new Error('Enter a name, valid coordinates, and a radius greater than zero.')
      if (form.priced && (!Number.isFinite(unit) || unit < 0 || !Number.isFinite(fee) || fee < 0))
        throw new Error('Charging prices cannot be negative.')
      const values = `${sqlString(form.name.trim())},${lat},${lng},${radius},${sqlString(form.billingType)},${form.priced ? unit : 'NULL'},${form.priced ? fee : 0}`
      const statement = editing === null
        ? `INSERT INTO geofences (name,latitude,longitude,radius_m,billing_type,cost_per_unit,session_fee) VALUES (${values})`
        : `UPDATE geofences SET (name,latitude,longitude,radius_m,billing_type,cost_per_unit,session_fee)=(${values}) WHERE id=${editing}`
      await runRemoteStatement(backend, statement)
      setEditing(null); setForm(empty); await load()
    } catch (reason: unknown) { setError(reason instanceof Error ? reason.message : 'Could not save geofence.') }
    finally { setBusy(false) }
  }

  const edit = (item: Geofence): void => {
    setEditing(item.id)
    setForm({ name: item.name, latitude: String(item.latitude), longitude: String(item.longitude), radiusM: String(item.radiusM), billingType: item.billingType, priced: item.costPerUnit !== null, costPerUnit: String(item.costPerUnit ?? 0), sessionFee: String(item.sessionFee) })
  }
  const remove = async (item: Geofence): Promise<void> => {
    if (!globalThis.confirm(`Delete geofence “${item.name}”?`)) return
    setBusy(true); setError(null)
    try { await runRemoteStatement(backend, `DELETE FROM geofences WHERE id=${item.id}`); await load() }
    catch (reason: unknown) { setError(reason instanceof Error ? reason.message : 'Could not delete geofence.') }
    finally { setBusy(false) }
  }

  const input = (key: keyof Form, label: string, type = 'text') => <label style={{ display: 'grid', gap: 5 }}>{label}<input type={type} value={String(form[key])} onChange={(e) => setForm({ ...form, [key]: e.target.value })} /></label>
  return <main style={{ maxWidth: 1100 }}>
    <h1>Geofences & charging prices</h1>
    <p>Changes are saved to Layerbase. The logger downloads them on its next cloud-sync cycle.</p>
    {error && <div className="error" role="alert">{error}</div>}
    <form onSubmit={(e) => void save(e)} style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(180px,1fr))', gap: 14, padding: 18, border: '1px solid var(--line)', borderRadius: 12 }}>
      {input('name', 'Name')}{input('latitude', 'Latitude', 'number')}{input('longitude', 'Longitude', 'number')}{input('radiusM', 'Radius (metres)', 'number')}
      <label style={{ display: 'grid', gap: 5 }}>Billing<select value={form.billingType} onChange={(e) => setForm({ ...form, billingType: e.target.value })}><option value="per_kwh">Per kWh</option><option value="per_minute">Per minute</option></select></label>
      <label><input type="checkbox" checked={form.priced} onChange={(e) => setForm({ ...form, priced: e.target.checked })} /> Set charging price</label>
      {form.priced && <>{input('costPerUnit', form.billingType === 'per_kwh' ? 'Cost per kWh' : 'Cost per minute', 'number')}{input('sessionFee', 'Session fee', 'number')}</>}
      <div><button type="submit" disabled={busy}>{editing === null ? 'Add geofence' : 'Save changes'}</button>{editing !== null && <button type="button" onClick={() => { setEditing(null); setForm(empty) }}>Cancel</button>}</div>
    </form>
    <table style={{ width: '100%', marginTop: 24 }}><thead><tr><th>Name</th><th>Coordinates</th><th>Radius</th><th>Charging price</th><th /></tr></thead><tbody>{items.map((item) => <tr key={item.id}><td>{item.name}</td><td>{item.latitude}, {item.longitude}</td><td>{item.radiusM} m</td><td>{item.costPerUnit === null ? 'Not set' : `${item.costPerUnit} / ${item.billingType === 'per_kwh' ? 'kWh' : 'minute'}${item.sessionFee ? ` + ${item.sessionFee} fee` : ''}`}</td><td><button type="button" onClick={() => edit(item)}>Edit</button> <button type="button" disabled={busy} onClick={() => void remove(item)}>Delete</button></td></tr>)}</tbody></table>
  </main>
}
