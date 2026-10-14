import { describe, expect, it } from 'vitest'
import { chargesCsv, drivesCsv } from './exportData'

describe('data export', () => {
  it('exports drives with stable columns and escaped locations', () => {
    const output = drivesCsv([{
      id: 7, time: '2026-10-14T12:00:00Z', from: 'Home, garage', to: 'Office "A"',
      distanceKm: 12.5, durationMin: 21, startBattery: 80, endBattery: 74,
      maxSpeedKmh: 95, maxPowerKw: 120, ascentM: 40, descentM: 35,
      outsideTempC: 18, averageSpeedKmh: 35.7, energyKwh: 2.1, rangeDiffKm: 14,
      carEfficiencyKwhKm: 0.168, hasReducedRange: 0,
    }])
    expect(output).toContain('id,start_time,from,to,distance_km')
    expect(output).toContain('"Home, garage","Office ""A"""')
    expect(output.endsWith('\r\n')).toBe(true)
  })

  it('keeps unavailable charging values empty instead of inventing zeroes', () => {
    const output = chargesCsv([{
      id: 3, time: '2026-10-13T20:00:00Z', location: 'Home', type: 'AC',
      startBattery: 30, endBattery: 80, energyAddedKwh: 35, energyUsedKwh: null,
      maxPowerKw: 11, cost: null, durationMin: 190, costPerKwh: null,
      efficiencyPercent: null, outsideTempC: 12, ratedRangeAddedKm: 220,
      idealRangeAddedKm: null, odometerKm: 9515,
    }])
    expect(output).toContain('energy_added_kwh,energy_used_kwh,max_power_kw,cost')
    expect(output).toContain(',35,,11,,190,')
  })
})
