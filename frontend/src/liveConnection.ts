// Live connection to a running teslalog instance.
//
// The viewer's original and only input was a file picker: you downloaded
// tesla.db from the portal and opened it by hand, and it went stale the
// moment you did. This connects directly instead.
//
// The polling shape matters more than it looks. /download takes a fresh
// SQLite snapshot on every request - measured on the Pi Zero 2 W at
// roughly 1000 ms and 10.1 MB - so polling it on a timer would be
// gigabytes a day of transfer and SD-card reads to observe data that
// changes a few times a day. /api/meta answers the same question in
// ~54 ms and ~100 bytes: how many closed drives and charges exist, and
// the highest position id. Only when one of those moves is the snapshot
// worth fetching again.
//
// gzip was measured and rejected: it cut 10.1 MB to 1.9 MB but cost
// 1711 ms of Pi CPU, which is the wrong trade on a machine whose whole
// job is to keep polling a car.

export type LiveMeta = Readonly<{
  lastUpdated: string
  sizeBytes: number
  drives: number
  charges: number
  latestPositionId: number
}>

export type LiveStatus = Readonly<{
  vehicleName: string
  state: string
  version: string
  batteryLevel: number | null
}>

export class LiveConnectionError extends Error {
  override readonly name = 'LiveConnectionError'
}

// normaliseBaseUrl accepts what a person actually types - "10.0.0.236",
// "10.0.0.236:8083", a full URL, a trailing slash - and returns an
// origin. Bare hosts default to http and teslalog's default port,
// because a teslalog portal on a home LAN is plain HTTP.
export const normaliseBaseUrl = (input: string): string => {
  const trimmed = input.trim().replace(/\/+$/u, '')
  if (trimmed === '') throw new LiveConnectionError('Enter the address of your teslalog portal.')
  const withScheme = /^https?:\/\//iu.test(trimmed) ? trimmed : `http://${trimmed}`
  let url: URL
  try {
    url = new URL(withScheme)
  } catch {
    throw new LiveConnectionError(`"${input}" is not a valid address.`)
  }
  if (url.port === '' && !/^https:/iu.test(withScheme)) url.port = '8083'
  return url.origin
}

// mixedContentBlocked reports the one failure mode that produces a
// bare, unexplained network error: an HTTPS page (the published viewer)
// cannot fetch a plain-HTTP LAN address, and the browser refuses before
// any request is made. Worth naming, because it is indistinguishable
// from "the Pi is off" in the raw error.
export const mixedContentBlocked = (baseUrl: string): boolean =>
  globalThis.location?.protocol === 'https:' && baseUrl.startsWith('http://')

const request = async (baseUrl: string, path: string, signal?: AbortSignal): Promise<Response> => {
  if (mixedContentBlocked(baseUrl))
    throw new LiveConnectionError(
      'This page is served over HTTPS and cannot reach a plain-HTTP address. Open the viewer from the teslalog portal itself, or run it locally.',
    )
  let response: Response
  try {
    response = await fetch(`${baseUrl}${path}`, signal === undefined ? {} : { signal })
  } catch (reason: unknown) {
    if (reason instanceof DOMException && reason.name === 'AbortError') throw reason
    throw new LiveConnectionError(
      `Could not reach ${baseUrl}. Check that teslalog is running and that this machine can see it.`,
    )
  }
  if (!response.ok)
    throw new LiveConnectionError(`${baseUrl}${path} returned HTTP ${response.status}.`)
  return response
}

export const fetchStatus = async (baseUrl: string, signal?: AbortSignal): Promise<LiveStatus> => {
  const body: unknown = await (await request(baseUrl, '/api/status', signal)).json()
  const record = body as Record<string, unknown>
  return {
    vehicleName: typeof record.vehicle_name === 'string' ? record.vehicle_name : 'Vehicle',
    state: typeof record.state === 'string' ? record.state : 'unknown',
    version: typeof record.version === 'string' ? record.version : 'unknown',
    batteryLevel: typeof record.battery_level === 'number' ? record.battery_level : null,
  }
}

export const fetchMeta = async (baseUrl: string, signal?: AbortSignal): Promise<LiveMeta> => {
  const body: unknown = await (await request(baseUrl, '/api/meta', signal)).json()
  const record = body as Record<string, unknown>
  const count = (value: unknown): number => (typeof value === 'number' ? value : 0)
  return {
    lastUpdated: typeof record.last_updated === 'string' ? record.last_updated : '',
    sizeBytes: count(record.size_bytes),
    drives: count(record.drives),
    charges: count(record.charges),
    latestPositionId: count(record.latest_position_id),
  }
}

export const fetchSnapshot = async (
  baseUrl: string,
  signal?: AbortSignal,
): Promise<Uint8Array> =>
  new Uint8Array(await (await request(baseUrl, '/download', signal)).arrayBuffer())

// hasNewData compares two meta readings. Drives and charges count only
// CLOSED rows, so they tick exactly when new history becomes available;
// latestPositionId moves continuously during a drive, which is what
// makes an in-progress drive visible without waiting for it to end.
export const hasNewData = (previous: LiveMeta | null, next: LiveMeta): boolean =>
  previous === null ||
  previous.drives !== next.drives ||
  previous.charges !== next.charges ||
  previous.latestPositionId !== next.latestPositionId

// 60 s. /api/meta costs ~54 ms and ~100 bytes on the Pi, so this is
// roughly 0.1% of one core and 144 KB a day - small enough not to
// matter, frequent enough that a finished drive shows up within a
// minute.
export const pollIntervalMs = 60_000

const storageKey = 'teslalog.viewer.liveUrl'

export const rememberedUrl = (): string => {
  try {
    return globalThis.localStorage?.getItem(storageKey) ?? ''
  } catch {
    return '' // Storage can be disabled outright; not worth failing over.
  }
}

export const rememberUrl = (baseUrl: string): void => {
  try {
    globalThis.localStorage?.setItem(storageKey, baseUrl)
  } catch {
    // Ignored: remembering the address is a convenience, not a feature.
  }
}
