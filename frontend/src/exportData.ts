import type { ChargeRow, DriveRow } from './domain'

const csvCell = (value: string | number | null): string => {
  if (value === null) return ''
  const text = String(value)
  return /[",\r\n]/u.test(text) ? `"${text.replaceAll('"', '""')}"` : text
}

const csv = (headers: readonly string[], rows: readonly (readonly (string | number | null)[])[]): string =>
  [headers, ...rows].map((row) => row.map(csvCell).join(',')).join('\r\n') + '\r\n'

export const drivesCsv = (drives: readonly DriveRow[]): string =>
  csv(
    [
      'id', 'start_time', 'from', 'to', 'distance_km', 'duration_min',
      'start_battery_percent', 'end_battery_percent', 'max_speed_kmh', 'max_power_kw',
      'ascent_m', 'descent_m', 'outside_temp_c', 'average_speed_kmh', 'energy_kwh',
      'range_used_km', 'efficiency_kwh_per_km', 'reduced_range',
    ],
    drives.map((drive) => [
      drive.id, drive.time, drive.from, drive.to, drive.distanceKm, drive.durationMin,
      drive.startBattery, drive.endBattery, drive.maxSpeedKmh, drive.maxPowerKw,
      drive.ascentM, drive.descentM, drive.outsideTempC, drive.averageSpeedKmh,
      drive.energyKwh, drive.rangeDiffKm, drive.carEfficiencyKwhKm, drive.hasReducedRange,
    ]),
  )

export const chargesCsv = (charges: readonly ChargeRow[]): string =>
  csv(
    [
      'id', 'start_time', 'location', 'type', 'start_battery_percent',
      'end_battery_percent', 'energy_added_kwh', 'energy_used_kwh', 'max_power_kw',
      'cost', 'duration_min', 'cost_per_kwh', 'efficiency_percent', 'outside_temp_c',
      'rated_range_added_km', 'ideal_range_added_km', 'odometer_km',
    ],
    charges.map((charge) => [
      charge.id, charge.time, charge.location, charge.type, charge.startBattery,
      charge.endBattery, charge.energyAddedKwh, charge.energyUsedKwh, charge.maxPowerKw,
      charge.cost, charge.durationMin, charge.costPerKwh, charge.efficiencyPercent,
      charge.outsideTempC, charge.ratedRangeAddedKm, charge.idealRangeAddedKm,
      charge.odometerKm,
    ]),
  )

export const download = (name: string, contents: string | Uint8Array, type: string): void => {
  const part: BlobPart = typeof contents === 'string'
    ? contents
    : (() => {
        const copy = new ArrayBuffer(contents.byteLength)
        new Uint8Array(copy).set(contents)
        return copy
      })()
  const url = URL.createObjectURL(new Blob([part], { type }))
  const link = document.createElement('a')
  link.href = url
  link.download = name
  link.click()
  setTimeout(() => URL.revokeObjectURL(url), 0)
}
