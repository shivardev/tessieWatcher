import { z } from 'zod'
export const vehicleSchema = z.object({
  displayName: z.string(),
  model: z.string(),
  firmware: z.string(),
  state: z.string(),
  battery: z.number().nullable(),
  rangeKm: z.number().nullable(),
  odometerKm: z.number().nullable(),
  drives: z.number().int(),
  distanceKm: z.number(),
  charges: z.number().int(),
  energyKwh: z.number(),
})
export type Vehicle = z.infer<typeof vehicleSchema>
export const driveRowSchema = z.object({
  id: z.number().int(),
  time: z.string(),
  from: z.string(),
  to: z.string(),
  distanceKm: z.number().nullable(),
  durationMin: z.number().nullable(),
  startBattery: z.number().nullable(),
  endBattery: z.number().nullable(),
  maxSpeedKmh: z.number().nullable(),
  ascentM: z.number().nullable(),
  descentM: z.number().nullable(),
  outsideTempC: z.number().nullable(),
  averageSpeedKmh: z.number().nullable(),
  energyKwh: z.number().nullable(),
  rangeDiffKm: z.number().nullable(),
  carEfficiencyKwhKm: z.number().nullable(),
})
export type DriveRow = z.infer<typeof driveRowSchema>
export const chargeRowSchema = z.object({
  id: z.number().int(),
  time: z.string(),
  location: z.string(),
  type: z.enum(['AC', 'DC']),
  startBattery: z.number().nullable(),
  endBattery: z.number().nullable(),
  energyAddedKwh: z.number().nullable(),
  energyUsedKwh: z.number().nullable(),
  maxPowerKw: z.number().nullable(),
  cost: z.number().nullable(),
  durationMin: z.number().nullable(),
  costPerKwh: z.number().nullable(),
  efficiencyPercent: z.number().nullable(),
  outsideTempC: z.number().nullable(),
})
export type ChargeRow = z.infer<typeof chargeRowSchema>
export type Metric = Readonly<{ label: string; value: number; unit: string }>
export type Destination = Readonly<{ name: string; visits: number }>
export type LoadedDatabase = Readonly<{
  fileName: string
  fileSize: number
  databaseBytes: Uint8Array
  vehicle: Vehicle
  drives: readonly DriveRow[]
  driveMetrics: readonly Metric[]
  lifetimeDriveMetrics: readonly Metric[]
  destinations: readonly Destination[]
  charges: readonly ChargeRow[]
  chargeMetrics: readonly Metric[]
}>
export const queryValueSchema = z.union([
  z.string(),
  z.number(),
  z.null(),
  z.instanceof(Uint8Array),
])
export type QueryValue = z.infer<typeof queryValueSchema>
export type QueryResult = Readonly<{
  columns: readonly string[]
  rows: readonly (readonly QueryValue[])[]
}>
export const groups = [
  { label: 'Overview', items: ['Overview'] },
  { label: 'Driving', items: ['Drives', 'Drive stats', 'Efficiency', 'Trip'] },
  { label: 'Charging', items: ['Charges', 'Charging stats'] },
  {
    label: 'Battery',
    items: ['Charge level', 'Battery health', 'Projected range', 'Vampire drain'],
  },
  { label: 'Places', items: ['Locations', 'Visited'] },
  { label: 'History', items: ['States', 'Timeline', 'Statistics', 'Mileage', 'Updates'] },
  { label: 'System', items: ['Database information'] },
] as const
export type Dashboard = (typeof groups)[number]['items'][number]
