import { timestampDate } from './viewSettings'

// Crosshair synchronisation between charts that share a time axis.
//
// Recharts' built-in syncMethod="value" matches by exact string equality
// on the axis value (`String(tick.value) === label`). teslalog's charts
// cannot satisfy that, for three compounding reasons:
//
//  1. Position rows come from two writers with different timestamp
//     precision. Streaming samples are written to the millisecond
//     ("2026-08-24T20:43:12.583Z"); poll-derived samples keep full
//     nanosecond RFC3339 ("2026-08-24T20:43:12.583710729Z").
//  2. The two writers carry different columns. On a real 4096-position
//     drive, 3265 rows were streaming (speed, elevation, no temperature)
//     and 478 were polled (temperature, no elevation) - so the
//     Temperatures chart and the Elevation chart are built from
//     completely disjoint sets of timestamps and could never match.
//  3. Each chart downsamples independently to a point budget, so the
//     strides differ (13 vs 12 vs 2 on that drive) and even two charts
//     over the same rows keep different instants.
//
// The result was a crosshair that followed between some pairs of charts
// and not others, which reads as flakiness rather than as a rule.
//
// Matching on nearest time instead of identical text fixes all three:
// the charts are describing the same drive, so "what was happening at
// this moment" is the question actually being asked.

// Parsing the same timestamp strings on every mouse move is wasteful -
// the same few hundred strings recur for as long as a chart is on
// screen - so parsed values are cached. The cache is cleared wholesale
// rather than evicted per entry: it is a pure function of the string,
// so dropping it costs one re-parse and nothing else.
const parsedEpoch = new Map<string, number>()
const maxCacheEntries = 20_000

export const epochMs = (value: unknown): number => {
  if (typeof value === 'number') return value
  if (typeof value !== 'string') return Number.NaN
  const cached = parsedEpoch.get(value)
  if (cached !== undefined) return cached
  const parsed = timestampDate(value).getTime()
  if (parsedEpoch.size >= maxCacheEntries) parsedEpoch.clear()
  parsedEpoch.set(value, parsed)
  return parsed
}

type SyncTick = Readonly<{ value?: unknown }>
type SyncSource = Readonly<{ activeLabel?: string | number | undefined }>

// syncTolerance bounds how far the nearest sample may be from the
// cursor before the crosshair is suppressed instead of snapped.
//
// Scaled to the chart's own sampling rather than a fixed number of
// seconds, because the same function serves drive telemetry (samples
// milliseconds apart) and 90-day catalog dashboards (samples hours
// apart); any fixed threshold is wrong for one of them. Without a bound
// at all, hovering past the end of a short series pins its crosshair to
// the last point and claims a reading that moment does not have.
export const syncTolerance = (ticks: readonly SyncTick[]): number => {
  const first = epochMs(ticks[0]?.value)
  const last = epochMs(ticks[ticks.length - 1]?.value)
  if (ticks.length < 2 || Number.isNaN(first) || Number.isNaN(last)) return Number.POSITIVE_INFINITY
  const spacing = Math.abs(last - first) / (ticks.length - 1)
  // Two samples of slack, and never less than a second, so ordinary
  // jitter between the two writers never drops the crosshair.
  return Math.max(spacing * 2, 1_000)
}

// nearestTimeSync returns the index of the tick closest in time to the
// hovered point on another chart, or -1 for "do not show a crosshair".
// Recharts indexes its tick array with the return value, so this is an
// array position, not a TickItem.index.
export const nearestTimeSync = (ticks: readonly SyncTick[], source: SyncSource): number => {
  const target = epochMs(source.activeLabel)
  if (Number.isNaN(target)) return -1

  let bestIndex = -1
  let bestGap = Number.POSITIVE_INFINITY
  for (const [index, tick] of ticks.entries()) {
    const value = epochMs(tick?.value)
    if (Number.isNaN(value)) continue
    const gap = Math.abs(value - target)
    if (gap < bestGap) {
      bestGap = gap
      bestIndex = index
    }
  }
  return bestGap <= syncTolerance(ticks) ? bestIndex : -1
}
