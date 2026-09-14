// Live connection to a running teslalog instance.
//
// The viewer's original and only input was a file picker: you downloaded
// tesla.db from the portal and opened it by hand, and it went stale the
// moment you did. This connects directly instead.
//
// The polling shape matters more than it looks. /download serves a
// pre-built, gzip-compressed snapshot (built only when a trip closes), so
// the transfer is cheap but still tens of MB. /api/meta answers "would a
// download get me anything new?" in ~54 ms and ~100 bytes. The one field
// that answers it correctly is snapshot_revision: the content hash of the
// snapshot /download would serve right now. It changes only when the
// served bytes change and never during a drive, so the viewer re-downloads
// exactly when there is new history - not once a minute while the car is
// moving, which is what keying on the live position id used to cause.
//
// drives/charges/latestPositionId are kept for the live status line (and
// so an older portal without snapshot_revision still animates), but they
// no longer drive the re-download decision.

export type LiveMeta = Readonly<{
  lastUpdated: string
  sizeBytes: number
  drives: number
  charges: number
  latestPositionId: number
  // Content hash of the snapshot /download would serve. "" from an older
  // portal that predates this field - see hasNewData for the fallback.
  snapshotRevision: string
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

// parseJson rejects a non-JSON body as a connection failure rather than
// letting a SyntaxError escape. A dev server (and many reverse proxies)
// answer an unknown path with index.html and HTTP 200, so "this origin
// is not a teslalog portal" arrives as valid HTML, not as an error.
const parseJson = async (response: Response, baseUrl: string): Promise<Record<string, unknown>> => {
  const text = await response.text()
  try {
    return JSON.parse(text) as Record<string, unknown>
  } catch {
    throw new LiveConnectionError(`${baseUrl} responded, but not with teslalog's JSON API.`)
  }
}

export const fetchStatus = async (baseUrl: string, signal?: AbortSignal): Promise<LiveStatus> => {
  const response = await request(baseUrl, '/api/status', signal)
  const record = await parseJson(response, baseUrl)
  return {
    vehicleName: typeof record.vehicle_name === 'string' ? record.vehicle_name : 'Vehicle',
    state: typeof record.state === 'string' ? record.state : 'unknown',
    version: typeof record.version === 'string' ? record.version : 'unknown',
    batteryLevel: typeof record.battery_level === 'number' ? record.battery_level : null,
  }
}

export const fetchMeta = async (baseUrl: string, signal?: AbortSignal): Promise<LiveMeta> => {
  const response = await request(baseUrl, '/api/meta', signal)
  const record = await parseJson(response, baseUrl)
  const count = (value: unknown): number => (typeof value === 'number' ? value : 0)
  return {
    lastUpdated: typeof record.last_updated === 'string' ? record.last_updated : '',
    sizeBytes: count(record.size_bytes),
    drives: count(record.drives),
    charges: count(record.charges),
    latestPositionId: count(record.latest_position_id),
    snapshotRevision: typeof record.snapshot_revision === 'string' ? record.snapshot_revision : '',
  }
}

export const fetchSnapshot = async (
  baseUrl: string,
  signal?: AbortSignal,
): Promise<Uint8Array> =>
  new Uint8Array(await (await request(baseUrl, '/download', signal)).arrayBuffer())

// hasNewData decides whether to re-download the snapshot. It keys on
// snapshot_revision - the hash of the bytes /download would serve - so it
// fires exactly when those bytes change and never re-fetches an unchanged
// snapshot, including the every-minute churn that keying on the live
// position id used to cause during a drive. When the portal is too old to
// report a revision (both sides ""), fall back to the closed-row counters
// so such a portal still updates on a finished trip; the position id is
// deliberately excluded so it cannot drive a re-download.
export const hasNewData = (previous: LiveMeta | null, next: LiveMeta): boolean => {
  if (previous === null) return true
  if (next.snapshotRevision !== '' || previous.snapshotRevision !== '')
    return previous.snapshotRevision !== next.snapshotRevision
  return previous.drives !== next.drives || previous.charges !== next.charges
}

// 60 s. /api/meta costs ~54 ms and ~100 bytes on the Pi, so this is
// roughly 0.1% of one core and 144 KB a day - small enough not to
// matter, frequent enough that a finished drive shows up within a
// minute.
export const pollIntervalMs = 60_000

// hostingOrigin is the origin this page was served from, when that could
// plausibly be a teslalog portal. The portal embeds this viewer at /app
// and serves the API from the same origin, so a viewer opened that way
// can connect to itself with nothing configured and no address typed -
// which is the only arrangement where the live connection works at all,
// since an HTTPS page cannot fetch a plain-HTTP LAN address.
//
// Returns null for a file:// page (no origin to speak of). It does NOT
// exclude the dev server: probing it costs one request that fails
// harmlessly, and excluding it by port would also exclude a real portal
// behind a proxy on the same port.
export const hostingOrigin = (): string | null => {
  const origin = globalThis.location?.origin
  return origin === undefined || origin === 'null' || origin.startsWith('file:') ? null : origin
}

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
