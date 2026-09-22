// Proof-of-concept harness for querying a Layerbase cloud database over
// its HTTP API instead of downloading the whole SQLite file. Reached at
// ?cloud - deliberately separate from the main App so it changes nothing
// about the normal viewer while we validate two things against a real
// database: that the dashboards render correctly over the HTTP API, and
// that it is actually fast (each dashboard fires several round-trips).
//
// The API key is entered here at runtime and kept only in this browser
// (optionally remembered in localStorage) - it is never part of the built
// bundle, which is what keeps a public GitHub Pages deploy from leaking it.
import { useState } from 'react'
import { GenericDashboard } from './GenericDashboard'
import { catalogDashboardKeys } from './dashboardRegistry'
import { setRemoteBackend, runRemoteQuery, type RemoteConfig } from './remoteBackend'
import { defaultViewSettings } from './viewSettings'
import type { QueryResult } from './domain'

const KEY_STORE = 'teslalog.viewer.layerbase'
const EMPTY = new Uint8Array(0)

const loadSaved = (): Partial<RemoteConfig> => {
  try {
    return JSON.parse(globalThis.localStorage?.getItem(KEY_STORE) ?? '{}') as Partial<RemoteConfig>
  } catch {
    return {}
  }
}

export function CloudProbe() {
  const saved = loadSaved()
  const [baseUrl, setBaseUrl] = useState(saved.baseUrl ?? 'https://sage.cloud.layerbase.dev')
  const [databaseId, setDatabaseId] = useState(saved.databaseId ?? '')
  const [apiKey, setApiKey] = useState(saved.apiKey ?? '')
  const [remember, setRemember] = useState(false)
  const [connected, setConnected] = useState(false)

  const [sql, setSql] = useState('SELECT COUNT(*) AS drives FROM drives')
  const [rawResult, setRawResult] = useState<QueryResult | null>(null)
  const [rawMs, setRawMs] = useState<number | null>(null)
  const [running, setRunning] = useState(false)

  const [catalogKey, setCatalogKey] = useState<string>(Object.values(catalogDashboardKeys)[0] ?? '')

  // Trim every field: a stray space or newline pasted with the key or id
  // is the classic cause of a spurious "Invalid API key".
  const config = (): RemoteConfig => ({
    baseUrl: baseUrl.trim(),
    databaseId: databaseId.trim(),
    apiKey: apiKey.trim(),
  })

  const connect = (): void => {
    setRemoteBackend(config())
    setConnected(true)
    if (remember) {
      try {
        globalThis.localStorage?.setItem(KEY_STORE, JSON.stringify(config()))
      } catch {
        /* storage disabled; the fields just won't be remembered */
      }
    }
  }

  const runRaw = async (): Promise<void> => {
    setRunning(true)
    const started = performance.now()
    const result = await runRemoteQuery(config(), sql)
    setRawMs(Math.round(performance.now() - started))
    setRawResult(result)
    setRunning(false)
  }

  const wrap: React.CSSProperties = { maxWidth: 1100, margin: '0 auto', padding: 24, color: '#e8efed' }
  const field: React.CSSProperties = {
    width: '100%',
    padding: 8,
    background: '#111819',
    color: '#e8efed',
    border: '1px solid #26322f',
    borderRadius: 6,
    marginTop: 4,
  }
  const label: React.CSSProperties = { display: 'block', fontSize: 12, color: '#879491', marginTop: 12 }

  return (
    <div style={{ minHeight: '100vh', background: '#0a1011' }}>
      <div style={wrap}>
        <h1 style={{ fontWeight: 600 }}>teslalog · cloud query probe</h1>
        <p style={{ color: '#879491' }}>
          Query a Layerbase database over HTTP — no file download. Your key stays in this browser.
        </p>

        <div
          style={{
            display: 'grid',
            gridTemplateColumns: '1fr 1fr',
            gap: 12,
            border: '1px solid #26322f',
            borderRadius: 12,
            padding: 16,
            background: '#121a1b',
          }}
        >
          <div style={{ gridColumn: '1 / -1' }}>
            <label style={label}>
              API base URL
              <input style={field} value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} />
            </label>
          </div>
          <label style={label}>
            Database id (UUID)
            <input
              style={field}
              value={databaseId}
              placeholder="8bf46fc7-…"
              onChange={(e) => setDatabaseId(e.target.value)}
            />
          </label>
          <label style={label}>
            API key (sk_…)
            <input
              style={field}
              type="password"
              value={apiKey}
              placeholder="sk_…"
              onChange={(e) => setApiKey(e.target.value)}
            />
          </label>
          <label style={{ ...label, display: 'flex', alignItems: 'center', gap: 8 }}>
            <input type="checkbox" checked={remember} onChange={(e) => setRemember(e.target.checked)} />
            Remember in this browser
          </label>
          <div style={{ display: 'flex', alignItems: 'flex-end' }}>
            <button
              type="button"
              onClick={connect}
              disabled={databaseId === '' || apiKey === ''}
              style={{
                background: '#c9ff43',
                color: '#111900',
                border: 0,
                borderRadius: 8,
                padding: '10px 16px',
                fontWeight: 700,
                cursor: 'pointer',
              }}
            >
              {connected ? 'Reconnect' : 'Connect'}
            </button>
          </div>
        </div>

        {connected && (
          <>
            <section style={{ marginTop: 24 }}>
              <h2 style={{ fontSize: 18 }}>Raw query (confirms response shape + latency)</h2>
              <textarea style={{ ...field, minHeight: 70, fontFamily: 'monospace' }} value={sql} onChange={(e) => setSql(e.target.value)} />
              <button
                type="button"
                onClick={() => void runRaw()}
                disabled={running}
                style={{ marginTop: 8, padding: '8px 14px', borderRadius: 6, border: '1px solid #26322f', background: '#1a2422', color: '#e8efed', cursor: 'pointer' }}
              >
                {running ? 'Running…' : 'Run'}
              </button>
              {rawMs !== null && (
                <p style={{ color: '#879491' }}>
                  {rawMs} ms · {rawResult?.error ? `error: ${rawResult.error}` : `${rawResult?.rows.length ?? 0} rows`}
                </p>
              )}
              {rawResult && !rawResult.error && (
                <pre style={{ ...field, overflow: 'auto', maxHeight: 240 }}>
                  {JSON.stringify({ columns: rawResult.columns, rows: rawResult.rows.slice(0, 20) }, null, 2)}
                </pre>
              )}
            </section>

            <section style={{ marginTop: 24 }}>
              <h2 style={{ fontSize: 18 }}>Render a real dashboard over HTTP</h2>
              <select style={{ ...field, maxWidth: 320 }} value={catalogKey} onChange={(e) => setCatalogKey(e.target.value)}>
                {Object.entries(catalogDashboardKeys).map(([name, key]) => (
                  <option key={key} value={key}>
                    {name}
                  </option>
                ))}
              </select>
              <div style={{ marginTop: 12 }}>
                <GenericDashboard
                  key={catalogKey}
                  catalogKey={catalogKey}
                  databaseBytes={EMPTY}
                  settings={defaultViewSettings}
                />
              </div>
            </section>
          </>
        )}
      </div>
    </div>
  )
}
