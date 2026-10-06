// Remote query backend: run the viewer's SQL against the teslalog server
// over its HTTP query API, instead of loading a downloaded SQLite file
// into sql.js. The server translates the viewer's portable SQL for
// PostgreSQL, so the same dashboard definitions run unchanged and the viewer opens
// without downloading the whole database.
//
// API: POST {baseUrl}/v1/databases/{id}/query with an Authorization bearer
// token and a JSON body `{"query": "SELECT ..."}`.
// The success response's exact shape is not pinned down in the public
// docs, so normaliseResult below accepts the shapes such an API plausibly
// returns and coerces them into the viewer's QueryResult; the first real
// run against a live database confirms which one it is.
import { queryValueSchema, type QueryResult, type QueryValue } from './domain'

export type RemoteConfig = Readonly<{
  // Origin of the teslalog server, e.g. "http://100.x.y.z:8085".
  baseUrl: string
  // The configured database id ("teslalog" by default).
  databaseId: string
  // The HTTP API bearer token. Supplied by the user at runtime and
  // held only in their browser - never committed or baked into the bundle.
  apiKey: string
}>

// The active remote backend, or null to use the local sql.js path. Set by
// the UI when the user connects to a cloud database; read by
// database.executeQueries to decide where a query runs.
let current: RemoteConfig | null = null
const mutationListeners = new Set<() => void>()

export const setRemoteBackend = (config: RemoteConfig | null): void => {
  current = config
}
export const getRemoteBackend = (): RemoteConfig | null => current

// Dashboard data is derived from several server queries and therefore lives
// in React state after the initial load. Notify the app after every successful
// write so charge costs, geofences, and future editable fields are reflected
// immediately without requiring a browser refresh.
export const subscribeRemoteMutations = (listener: () => void): (() => void) => {
  mutationListeners.add(listener)
  return () => mutationListeners.delete(listener)
}

const notifyRemoteMutation = (): void => {
  for (const listener of mutationListeners) listener()
}

const queryEndpoint = (config: RemoteConfig): string =>
  `${config.baseUrl.replace(/\/+$/u, '')}/v1/databases/${encodeURIComponent(config.databaseId)}/query`

// coerce narrows an arbitrary JSON value to the QueryValue union the
// dashboards expect. SQLite has no boolean, so a JSON true/false (however
// the API chose to render an integer column) collapses back to 1/0; an
// object or array - which our SELECTs never produce - becomes its string
// form rather than crashing the row.
const coerce = (value: unknown): QueryValue => {
  if (value === null || value === undefined) return null
  if (typeof value === 'number') return value
  if (typeof value === 'boolean') return value ? 1 : 0
  if (typeof value === 'string') {
    // Compatible APIs may return SQLite numerics as JSON strings.
    // Restore strictly-numeric ones to numbers so the dashboards compute
    // rather than concatenate; dates, locations and states have letters or
    // punctuation and stay text. Zero-padded values (a "01234" postcode)
    // and integers beyond 2^53 keep their exact string form.
    if (/^-?\d+(?:\.\d+)?$/.test(value) && !/^-?0\d/.test(value)) {
      const n = Number(value)
      if (value.includes('.') || Number.isSafeInteger(n)) return n
    }
    // Exponent form from a large SUM, e.g. "1.23e+08".
    if (/^-?\d+(?:\.\d+)?[eE][+-]?\d+$/.test(value)) {
      const n = Number(value)
      if (Number.isFinite(n)) return n
    }
    // Thousands-separated numeric, e.g. "5,800.5". The \d{3} groups keep
    // this from matching text like "Home, Chattanooga" or "35.04, -85.15".
    if (/^-?\d{1,3}(?:,\d{3})+(?:\.\d+)?$/.test(value)) {
      return Number(value.replace(/,/g, ''))
    }
    return value
  }
  return queryValueSchema.catch(String(value)).parse(value)
}

// normaliseResult turns the server's JSON into a QueryResult, accepting the
// two shapes a SQLite-over-HTTP API realistically returns:
//   1. { columns: ["a","b"], rows: [[1,2],[3,4]] }
//   2. an array of row objects: [{ "a": 1, "b": 2 }, ...] - possibly
//      wrapped under `rows`, `results`, or `data`.
// Column order for shape 2 comes from the first row's key order, which
// SQLite result serialisers preserve from the SELECT list.
export const normaliseResult = (json: unknown): QueryResult => {
  if (json && typeof json === 'object' && !Array.isArray(json)) {
    const record = json as Record<string, unknown>
    if (Array.isArray(record.columns) && Array.isArray(record.rows)) {
      const columns = record.columns.map(String)
      const rows = (record.rows as unknown[]).map((row) =>
        Array.isArray(row)
          ? row.map(coerce)
          : columns.map((column) => coerce((row as Record<string, unknown>)?.[column])),
      )
      return { columns, rows }
    }
  }
  const array = Array.isArray(json)
    ? json
    : Array.isArray((json as Record<string, unknown>)?.rows)
      ? ((json as Record<string, unknown>).rows as unknown[])
      : Array.isArray((json as Record<string, unknown>)?.results)
        ? ((json as Record<string, unknown>).results as unknown[])
        : Array.isArray((json as Record<string, unknown>)?.data)
          ? ((json as Record<string, unknown>).data as unknown[])
          : null
  if (array) {
    if (array.length === 0) return { columns: [], rows: [] }
    const first = array[0]
    if (first !== null && typeof first === 'object' && !Array.isArray(first)) {
      const columns = Object.keys(first as Record<string, unknown>)
      const rows = array.map((entry) =>
        columns.map((column) => coerce((entry as Record<string, unknown>)?.[column])),
      )
      return { columns, rows }
    }
    return { columns: [], rows: array.map((row) => (Array.isArray(row) ? row.map(coerce) : [coerce(row)])) }
  }
  return { columns: [], rows: [], error: 'Unrecognised response shape from the teslalog data server.' }
}

// runRemoteQuery posts one already-interpolated SQL string and returns its
// result. A failure - network, auth, or a SQL error the API reports - is
// captured on the QueryResult (like the local path) so one bad panel does
// not blank the whole dashboard.
export const runRemoteQuery = async (
  config: RemoteConfig,
  sql: string,
  signal?: AbortSignal,
): Promise<QueryResult> => {
  let response: Response
  try {
    response = await fetch(queryEndpoint(config), {
      method: 'POST',
      headers: remoteHeaders(config),
      body: JSON.stringify({ query: sql }),
      ...(signal === undefined ? {} : { signal }),
    })
  } catch {
    // An unreachable address (VPN down, server off) otherwise hangs until
    // the browser's own connect timeout, so callers pass a timeout signal
    // and the two abort reasons get their own wording here.
    const reason: unknown = signal?.aborted ? signal.reason : undefined
    if (reason instanceof DOMException && reason.name === 'TimeoutError')
      return {
        columns: [],
        rows: [],
        error: `The teslalog data server at ${config.baseUrl} did not answer in time. Check the VPN connection and that the server is running.`,
      }
    if (reason !== undefined) return { columns: [], rows: [], error: 'Cancelled.' }
    return {
      columns: [],
      rows: [],
      error: 'Could not reach the teslalog data server from this browser. Check the VPN address and that the server is running.',
    }
  }
  if (!response.ok) {
    let message = `Data server returned HTTP ${response.status}.`
    try {
      const body = (await response.json()) as { error?: unknown }
      if (typeof body?.error === 'string') message = body.error
    } catch {
      // Non-JSON error body; the status-code message above stands.
    }
    return { columns: [], rows: [], error: message }
  }
  try {
    return normaliseResult(await response.json())
  } catch {
    return { columns: [], rows: [], error: 'Data server returned a non-JSON body.' }
  }
}

// runRemoteStatement executes a write statement. The server may return an
// empty success body for INSERT/UPDATE/DELETE, so unlike runRemoteQuery this
// intentionally cares only about the HTTP status.
export const runRemoteStatement = async (config: RemoteConfig, sql: string): Promise<void> => {
  let response: Response
  try {
    response = await fetch(queryEndpoint(config), {
      method: 'POST',
      headers: remoteHeaders(config),
      body: JSON.stringify({ query: sql }),
    })
  } catch {
    throw new Error('Could not reach the teslalog data server.')
  }
  if (!response.ok) {
    let message = `Data server returned HTTP ${response.status}.`
    try {
      const body = (await response.json()) as { error?: unknown }
      if (typeof body?.error === 'string') message = body.error
    } catch {
      // Keep the status message.
    }
    throw new Error(message)
  }
  notifyRemoteMutation()
}

const remoteHeaders = (config: RemoteConfig): Record<string, string> => {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  if (config.apiKey.trim() !== '') headers.Authorization = `Bearer ${config.apiKey}`
  return headers
}

// runRemoteQueries runs a dashboard's queries concurrently - the whole
// point of querying in place is that several small round-trips finish
// together instead of downloading the entire database first.
export const runRemoteQueries = async (
  config: RemoteConfig,
  queries: readonly string[],
): Promise<readonly QueryResult[]> => Promise.all(queries.map((sql) => runRemoteQuery(config, sql)))
