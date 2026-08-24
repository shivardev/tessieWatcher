import { describe, expect, it } from 'vitest'
import { convertLabel, convertValue, unitKey } from './GenericDashboard'
import type { ViewSettings } from './viewSettings'

// Unit-direction bugs have been the single most common defect in this
// viewer: four separate panels shipped dividing a per-distance rate that
// should have been multiplied, and one shipped a header reading "km"
// beside values already converted to miles. The errors are quiet - the
// number still looks like a number - so they are pinned here rather than
// left to be caught by eye.

const imperial: ViewSettings = { lengthUnit: 'mi', temperatureUnit: 'C', timeRange: 'all' }
const metric: ViewSettings = { lengthUnit: 'km', temperatureUnit: 'C', timeRange: 'all' }

describe('convertValue', () => {
  it('divides a plain distance', () => {
    expect(convertValue(1.60934, 'Distance (km)', imperial)).toBeCloseTo(1, 4)
  })

  // The bug: a rate expressed per kilometre gets BIGGER per mile, because a
  // mile is the longer unit. Dividing produced 130 Wh/mi for a car that
  // actually uses 209, and 0.16 %/mi for drives that plainly used 0.42.
  it.each([
    ['Wh/km', 130, 209.21],
    ['%/km', 0.263, 0.4233],
    ['Configured Wh/km (if set)', 130, 209.21],
  ])('multiplies the per-distance rate %s', (column, input, expected) => {
    expect(convertValue(input, column, imperial)).toBeCloseTo(expected, 2)
  })

  // Distance over distance is the same number in every unit.
  it('leaves a distance-per-distance ratio alone', () => {
    expect(convertValue(1.15, 'Avg rated-range km lost per km driven (90d)', imperial)).toBe(1.15)
  })

  it('treats km/h as a speed, not a per-distance rate', () => {
    expect(convertValue(100, 'Speed (km/h)', imperial)).toBeCloseTo(62.137, 2)
  })

  it('changes nothing when the viewer is metric', () => {
    for (const column of ['Wh/km', '%/km', 'Distance (km)', 'Speed (km/h)']) {
      expect(convertValue(42, column, metric)).toBe(42)
    }
  })
})

describe('convertLabel', () => {
  // "km lost / h" begins the string, so the old ' km' rule never matched
  // and the header contradicted the miles printed beneath it.
  it('rewrites a bare leading km', () => {
    expect(convertLabel('km lost / h', imperial)).toBe('mi lost / h')
  })

  it.each([
    ['Range loss (km)', 'Range loss (mi)'],
    ['Wh/km', 'Wh/mi'],
    ['Speed km/h', 'Speed mi/h'],
    ['rated km lost', 'rated mi lost'],
  ])('rewrites %s', (input, expected) => {
    expect(convertLabel(input, imperial)).toBe(expected)
  })
})

describe('unitKey', () => {
  // Every catalog stat panel aliases its column `value`; the unit only
  // exists in the panel title, so without this fallback the title said
  // Wh/mi while the number stayed in Wh/km.
  it('falls back to the panel title for a unitless column name', () => {
    expect(unitKey('value', 'Configured Wh/km (if set)')).toBe('Configured Wh/km (if set)')
  })

  it('prefers the column when the column itself carries the unit', () => {
    expect(unitKey('Distance (km)', 'Some panel about temperature')).toBe('Distance (km)')
  })
})
