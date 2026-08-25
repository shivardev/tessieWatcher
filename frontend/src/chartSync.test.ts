import { describe, expect, it } from 'vitest'
import { nearestTimeSync, syncTolerance } from './chartSync'

const ticks = (...values: readonly string[]) => values.map((value) => ({ value }))

// A real drive interleaves two writers. Streaming samples land on the
// millisecond and carry speed and elevation; poll-derived samples keep
// full nanosecond precision and carry temperature. On the drive this was
// measured against, 3265 rows were streaming and 478 were polled, and no
// timestamp appeared in both sets.
const streamed = ticks(
  '2026-08-24T20:43:12.583Z',
  '2026-08-24T20:43:16.095Z',
  '2026-08-24T20:43:19.179Z',
)
const polled = ticks(
  '2026-08-24T20:43:12.583710729Z',
  '2026-08-24T20:43:16.095923127Z',
  '2026-08-24T20:43:19.179589138Z',
)

describe('nearestTimeSync', () => {
  // The bug: Recharts' own syncMethod="value" compares tick values as
  // strings, so these two series - the same instants, written by two
  // code paths at different precision - never matched and the crosshair
  // silently refused to cross between those charts.
  it('matches across the precision difference exact string equality misses', () => {
    for (const [index, tick] of polled.entries()) {
      expect(nearestTimeSync(streamed, { activeLabel: tick.value })).toBe(index)
    }
  })

  it('snaps to the closest sample when the exact instant is absent', () => {
    // Half a second after the second sample, and well before the third.
    expect(nearestTimeSync(streamed, { activeLabel: '2026-08-24T20:43:16.600Z' })).toBe(1)
  })

  it('is symmetric between the two series', () => {
    expect(nearestTimeSync(polled, { activeLabel: streamed[2]!.value })).toBe(2)
  })

  // Without a bound, hovering past the end of a short series pins its
  // crosshair to the last point and asserts a reading for a moment that
  // series does not cover.
  it('suppresses the crosshair well outside the series', () => {
    expect(nearestTimeSync(streamed, { activeLabel: '2026-08-24T22:00:00.000Z' })).toBe(-1)
  })

  it.each([
    ['a non-time label', 'not a timestamp'],
    ['no label at all', undefined],
  ])('returns -1 for %s', (_label, activeLabel) => {
    expect(nearestTimeSync(streamed, { activeLabel })).toBe(-1)
  })

  it('returns -1 rather than 0 for an empty chart', () => {
    expect(nearestTimeSync([], { activeLabel: streamed[0]!.value })).toBe(-1)
  })
})

describe('syncTolerance', () => {
  // Scaled to the chart's own sampling: the same function serves drive
  // telemetry sampled milliseconds apart and 90-day dashboards sampled
  // hours apart, and any fixed threshold is wrong for one of them.
  it('grows with the spacing of the series', () => {
    const dense = ticks('2026-08-24T20:00:00Z', '2026-08-24T20:00:02Z', '2026-08-24T20:00:04Z')
    const sparse = ticks('2026-06-01T00:00:00Z', '2026-07-01T00:00:00Z', '2026-08-01T00:00:00Z')
    expect(syncTolerance(sparse)).toBeGreaterThan(syncTolerance(dense))
  })

  // A one-second floor, so ordinary jitter between the two writers never
  // drops the crosshair on a densely sampled chart.
  it('never falls below a second', () => {
    const veryDense = ticks('2026-08-24T20:00:00.000Z', '2026-08-24T20:00:00.100Z')
    expect(syncTolerance(veryDense)).toBe(1_000)
  })

  it('places no bound on a series too short to have a spacing', () => {
    expect(syncTolerance(ticks('2026-08-24T20:00:00Z'))).toBe(Number.POSITIVE_INFINITY)
  })
})
