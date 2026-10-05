import { describe, expect, it } from 'vitest'
import {
  celsiusToFahrenheit,
  formatTimeInput,
  kilometersPerHourToMilesPerHour,
  kilometersToMiles,
  parseTimeExpression,
  rangeFromInputs,
  shiftTimeRange,
  sqlDateTime,
  timeRangeInputs,
  timeRangeLabel,
  timeRangeSql,
  withinTimeWindow,
  zoomOutTimeRange,
} from './viewSettings'

describe('view settings', () => {
  it('converts canonical metric storage values for imperial display', () => {
    expect(kilometersToMiles(1.60934)).toBeCloseTo(1, 5)
    expect(kilometersPerHourToMilesPerHour(96.5604)).toBeCloseTo(60, 3)
    expect(celsiusToFahrenheit(20)).toBe(68)
  })

  it('creates bounded SQL predicates from an enum rather than user text', () => {
    expect(timeRangeSql('24h', 'start_time')).toBe("start_time >= datetime('now', '-24 hours')")
    expect(timeRangeSql('1y', 'start_time')).toBe("start_time >= datetime('now', '-1 year')")
    expect(timeRangeSql('15m', 'start_time')).toBe("start_time >= datetime('now', '-15 minutes')")
    expect(timeRangeSql('6M', 'start_time')).toBe("start_time >= datetime('now', '-6 months')")
    expect(timeRangeSql('all', 'start_time')).toBe('1 = 1')
  })

  it('bounds a custom date range by whole local days, both ends inclusive', () => {
    const filter = { timeRange: 'custom', customFrom: '2026-09-01', customTo: '2026-09-30' } as const
    const from = sqlDateTime(new Date(2026, 8, 1))
    const to = sqlDateTime(new Date(2026, 8, 30, 23, 59, 59))
    expect(timeRangeSql(filter, 'start_time')).toBe(
      `(start_time >= datetime('${from}') AND start_time <= datetime('${to}'))`,
    )
    expect(withinTimeWindow(new Date(2026, 8, 30, 23, 59).getTime(), filter)).toBe(true)
    expect(withinTimeWindow(new Date(2026, 9, 1, 0, 1).getTime(), filter)).toBe(false)
    expect(withinTimeWindow(new Date(2026, 7, 31, 23, 59).getTime(), filter)).toBe(false)
  })

  it('honours a time of day on either side of a custom range', () => {
    const filter = {
      timeRange: 'custom',
      customFrom: '2026-10-02 08:30',
      customTo: '2026-10-02 17:00:00',
    } as const
    expect(withinTimeWindow(new Date(2026, 9, 2, 8, 29).getTime(), filter)).toBe(false)
    expect(withinTimeWindow(new Date(2026, 9, 2, 12).getTime(), filter)).toBe(true)
    expect(withinTimeWindow(new Date(2026, 9, 2, 17, 1).getTime(), filter)).toBe(false)
  })

  it('parses Grafana-style relative expressions and rejects rollovers', () => {
    const now = new Date(2026, 9, 5, 12).getTime()
    expect(parseTimeExpression('now', 'to', now)?.getTime()).toBe(now)
    expect(parseTimeExpression('now-2d', 'from', now)).toEqual(new Date(2026, 9, 3, 12))
    expect(parseTimeExpression('now-6h', 'from', now)).toEqual(new Date(2026, 9, 5, 6))
    expect(parseTimeExpression('now-1M', 'from', now)).toEqual(new Date(2026, 8, 5, 12))
    expect(parseTimeExpression('2026-02-31', 'from', now)).toBeNull()
    expect(parseTimeExpression('2026-10-02 25:00', 'from', now)).toBeNull()
    expect(parseTimeExpression('yesterday', 'from', now)).toBeNull()
  })

  it('round-trips quick ranges through the From/To text fields', () => {
    expect(timeRangeInputs({ timeRange: '7d' })).toEqual({ from: 'now-7d', to: 'now' })
    expect(rangeFromInputs('now-7d', 'now').timeRange).toBe('7d')
    expect(rangeFromInputs(' now-6M ', 'now').timeRange).toBe('6M')
    expect(rangeFromInputs('now-9d', 'now')).toEqual({
      timeRange: 'custom',
      customFrom: 'now-9d',
      customTo: 'now',
    })
    expect(rangeFromInputs('', '').timeRange).toBe('all')
  })

  it('leaves an open or malformed custom side unbounded and never echoes raw input', () => {
    expect(timeRangeSql({ timeRange: 'custom', customFrom: '', customTo: '' }, 'start_time')).toBe(
      '1 = 1',
    )
    expect(
      timeRangeSql(
        { timeRange: 'custom', customFrom: "2026-01-01') OR 1=1 --", customTo: '' },
        'start_time',
      ),
    ).toBe('1 = 1')
    expect(timeRangeLabel({ timeRange: 'custom', customFrom: '', customTo: '' })).toBe('All time')
    expect(timeRangeLabel({ timeRange: '7d' })).toBe('Last 7 days')
  })

  it('shifts and zooms a window by its own length without passing now', () => {
    const now = new Date(2026, 9, 5, 12).getTime()
    const lastTwoDays = { timeRange: '2d' } as const
    expect(shiftTimeRange(lastTwoDays, -1, now)).toEqual({
      timeRange: 'custom',
      customFrom: formatTimeInput(new Date(2026, 9, 1, 12)),
      customTo: formatTimeInput(new Date(2026, 9, 3, 12)),
    })
    const earlier = {
      timeRange: 'custom',
      customFrom: '2026-10-01 12:00:00',
      customTo: '2026-10-03 12:00:00',
    } as const
    expect(shiftTimeRange(earlier, 1, now)).toEqual({
      timeRange: 'custom',
      customFrom: '2026-10-03 12:00:00',
      customTo: 'now',
    })
    expect(zoomOutTimeRange(lastTwoDays, now)).toEqual({
      timeRange: 'custom',
      customFrom: formatTimeInput(new Date(2026, 9, 1, 12)),
      customTo: 'now',
    })
    expect(shiftTimeRange({ timeRange: 'all' }, -1, now)).toBeNull()
  })
})
