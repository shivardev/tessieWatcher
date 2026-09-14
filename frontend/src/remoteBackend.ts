// Remote query backend: run the viewer's SQL against a Layerbase cloud
// database over its HTTP query API, instead of loading a downloaded
// SQLite file into sql.js. Layerbase is SQLite 3 under the hood, so the
// exact same dashboard SQL runs unchanged - only where it executes moves
// from the browser to the cloud, which is what lets the viewer open
// without downloading the whole database.
//
// The API (https://layerbase.com/docs): POST {baseUrl}/v1/databases/{id}/query
// with `Authorization: Bearer sk_...` and a JSON body `{"query": "SELECT ..."}`.
// The success response's exact shape is not pinned down in the public
// docs, so normaliseResult below accepts the shapes such an API plausibly
// returns and coerces them into the viewer's QueryResult; the first real
// run against a live database confirms which one it is.
import { queryValueSchema, type QueryResult, type QueryValue } from './domain'

export type RemoteConfig = Readonly<{
  // Origin of the cloud API, e.g. "https://cloud.layerbase.dev".
  baseUrl: string
  // The database's id (the UUID from the API-key panel / connection URL),
  // NOT the pooled hostname.
  databaseId: string
  // The HTTP API bearer token (sk_...). Supplied by the user at runtime and
  // held only in their browser - never committed or baked into the bundle.
  apiKey: string
}>

// The active remote backend, or null to use the local sql.js path. Set by
// the UI when the user connects to a cloud database; read by
// database.executeQueries to decide where a query runs.
let current: RemoteConfig | null = null
export const setRemoteBackend = (config: RemoteConfig | null): void => {
  current = config
}
export const getRemoteBackend = (): RemoteConfig | null => current

const queryEndpoint = (config: RemoteConfig): string =>
  `${config.baseUrl.replace(/\/+$/u, '')}/v1/databases/${encodeURIComponent(config.databaseId)}/query`

// coerce narrows an arbitrary JSON value to the QueryValue union the
// dashboards expect. SQLite has no boolean, so a JSON true/false (however
// the API chose to render an integer column) collapses back to 1/0; an
// object or array - which our SELECTs never produce - becomes its string
// form rather than crashing the row.
const coerce = (value: unknown): QueryValue => {
  if (value === null || value === undefined) return null
  if (typeof value === 'string' || typeof value === 'number') return value
  if (typeof value === 'boolean') return value ? 1 : 0
  return queryValueSchema.catch(String(value)).parse(value)
}

// normaliseResult turns Layerbase's JSON into a QueryResult, accepting the
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
  return { columns: [], rows: [], error: 'Unrecognised response shape from the Layerbase query API.' }
}

// runRemoteQuery posts one already-interpolated SQL string and returns its
// result. A failure - network, auth, or a SQL error the API reports - is
// captured on the QueryResult (like the local path) so one bad panel does
// not blank the whole dashboard.
export const runRemoteQuery = async (config: RemoteConfig, sql: string): Promise<QueryResult> => {
  let response: Response
  try {
    response = await fetch(queryEndpoint(config), {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${config.apiKey}`,
      },
      body: JSON.stringify({ query: sql }),
    })
  } catch {
    return { columns: [], rows: [], error: 'Could not reach the Layerbase query API.' }
  }
  if (!response.ok) {
    let message = `Layerbase query API returned HTTP ${response.status}.`
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
    return { columns: [], rows: [], error: 'Layerbase query API returned a non-JSON body.' }
  }
}

// runRemoteQueries runs a dashboard's queries concurrently - the whole
// point of querying in place is that several small round-trips finish
// together instead of downloading the entire database first.
export const runRemoteQueries = async (
  config: RemoteConfig,
  queries: readonly string[],
): Promise<readonly QueryResult[]> => Promise.all(queries.map((sql) => runRemoteQuery(config, sql)))
