import { describe, expect, it } from 'vitest'
import { axisTicks, humanDuration, spansFromRows, stateColor } from './StateTimeline'

describe('stateColor', () => {
  it('is case- and whitespace-insensitive', () => {
    expect(stateColor('Driving')).toBe(stateColor(' driving '))
  })

  // The old implementation keyed colours off CSS classes built from a
  // mangled state string, so an unrecognised state produced no class and
  // rendered as an invisible gap in the track - indistinguishable from
  // missing data. An unknown state must be visibly *something*.
  it('gives an unknown state a real colour rather than nothing', () => {
    const unknown = stateColor('some future state')
    expect(unknown).toMatch(/^#[0-9a-f]{6}$/iu)
    expect(unknown).not.toBe(stateColor('driving'))
  })

  it('separates AC from DC charging', () => {
    expect(stateColor('charging (dc)')).not.toBe(stateColor('charging (ac)'))
  })
})

describe('humanDuration', () => {
  it.each([
    [15_000, '15s'],
    [90_000, '2m'],
    [45 * 60_000, '45m'],
    [60 * 60_000, '1h'],
    [95 * 60_000, '1h 35m'],
    [24 * 60 * 60_000, '1d'],
    [(3 * 24 + 6) * 60 * 60_000, '3d 6h'],
  ])('formats %ims as %s', (ms, expected) => {
    expect(humanDuration(ms)).toBe(expected)
  })

  // A car parked for three days is the common case in this data; showing
  // that as "4680m" would be useless, and it is the single largest
  // legend entry.
  it('never reports a multi-day span in minutes', () => {
    expect(humanDuration(5 * 24 * 60 * 60_000)).not.toMatch(/m$/u)
  })
})

describe('axisTicks', () => {
  // Ticks have to land on times a person recognises. Aligning to the
  // window start instead would label an all-day view at 09:37, 15:37,
  // 21:37 - technically spaced, useless to read against.
  it('aligns sub-day ticks to the top of the hour', () => {
    const start = new Date(2026, 7, 25, 9, 37, 24).getTime()
    const ticks = axisTicks(start, start + 12 * 60 * 60_000)
    expect(ticks.length).toBeGreaterThan(1)
    for (const at of ticks) {
      const date = new Date(at)
      expect(date.getMinutes()).toBe(0)
      expect(date.getSeconds()).toBe(0)
    }
  })

  // Local midnight, not a multiple of 86400000 from the epoch - those
  // differ everywhere outside UTC, and by 30 minutes in some zones.
  it('aligns multi-day ticks to local midnight', () => {
    const start = new Date(2026, 7, 20, 14, 5).getTime()
    const ticks = axisTicks(start, start + 10 * 24 * 60 * 60_000)
    expect(ticks.length).toBeGreaterThan(1)
    for (const at of ticks) {
      expect(new Date(at).getHours()).toBe(0)
    }
  })

  it('never emits a tick before the window starts', () => {
    const start = new Date(2026, 7, 25, 9, 37).getTime()
    for (const at of axisTicks(start, start + 6 * 60 * 60_000)) {
      expect(at).toBeGreaterThanOrEqual(start)
    }
  })

  it('returns nothing for an empty or inverted window', () => {
    const now = Date.now()
    expect(axisTicks(now, now)).toEqual([])
    expect(axisTicks(now, now - 1000)).toEqual([])
  })
})

describe('spansFromRows', () => {
  // The catalog's state-timeline queries return one row per state CHANGE,
  // written as a plain "time, state" series so the same SQL also works in
  // Grafana. Each row's end is the next row's start.
  it('turns change points into contiguous spans', () => {
    const spans = spansFromRows(
      [
        ['2026-08-25T10:00:00Z', 'asleep'],
        ['2026-08-25T11:00:00Z', 'driving'],
      ],
      Date.parse('2026-08-25T12:00:00Z'),
    )
    expect(spans).toHaveLength(2)
    expect(spans[0]!.end).toBe(spans[1]!.start)
    // The final, still-open state runs to now rather than to zero width.
    expect(spans[1]!.end).toBe(Date.parse('2026-08-25T12:00:00Z'))
  })

  it('skips rows with an unusable time or state', () => {
    const spans = spansFromRows([
      ['not a date', 'driving'],
      ['2026-08-25T10:00:00Z', null],
      ['2026-08-25T11:00:00Z', 'asleep'],
    ])
    expect(spans).toHaveLength(1)
    expect(spans[0]!.state).toBe('asleep')
  })

  it('accepts epoch-millisecond times as well as strings', () => {
    const start = Date.parse('2026-08-25T10:00:00Z')
    const spans = spansFromRows([[start, 'driving']], start + 60_000)
    expect(spans[0]!.start).toBe(start)
  })
})

// An axis with one label is as useless as no axis. Every window from a
// single drive to a year has to produce a readable number of ticks.
describe('axisTicks density', () => {
  it.each([
    ['20 minutes', 20 * 60_000],
    ['2 hours', 2 * 60 * 60_000],
    ['12 hours', 12 * 60 * 60_000],
    ['3 days', 3 * 24 * 60 * 60_000],
    ['10 days', 10 * 24 * 60 * 60_000],
    ['90 days', 90 * 24 * 60 * 60_000],
    ['1 year', 365 * 24 * 60 * 60_000],
  ])('produces a readable number of ticks over %s', (_label, span) => {
    const start = new Date(2026, 7, 20, 14, 5).getTime()
    const ticks = axisTicks(start, start + span)
    expect(ticks.length).toBeGreaterThanOrEqual(3)
    expect(ticks.length).toBeLessThanOrEqual(14)
  })
})
