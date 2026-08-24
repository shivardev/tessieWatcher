import { describe, expect, it } from 'vitest'
import {
  celsiusToFahrenheit,
  kilometersPerHourToMilesPerHour,
  kilometersToMiles,
  timeRangeSql,
} from './viewSettings'

describe('view settings', () => {
  it('converts canonical metric storage values for imperial display', () => {
    expect(kilometersToMiles(1.60934)).toBeCloseTo(1, 5)
    expect(kilometersPerHourToMilesPerHour(96.5604)).toBeCloseTo(60, 3)
    expect(celsiusToFahrenheit(20)).toBe(68)
  })

  it('creates bounded SQL predicates from an enum rather than user text', () => {
    expect(timeRangeSql('24h', 'start_time')).toBe("start_time >= datetime('now', '-24 hours')")
    expect(timeRangeSql('all', 'start_time')).toBe('1 = 1')
  })
})
