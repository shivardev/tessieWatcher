import { describe, expect, it } from 'vitest'
import type { ChargeRow } from './domain'
import {
  costUpdateSql,
  parseAmount,
  parseCsv,
  parseSuperchargerCsv,
  planImport,
  resultingCost,
} from './superchargerCsv'

// Same header and value formats as Tesla's export; names, VINs and invoices invented.
const header =
  'ChargeStartDateTime,Name,Vin,Model,Country,SiteLocationName,Description,QuantityBase,QuantityTier1,QuantityTier2,QuantityTier3,QuantityTier4,InvoiceNumber,UnitCostBase,UnitCostTier1,UnitCostTier2,UnitCostTier3,UnitCostTier4,VAT,Total Exc. VAT,Total Inc. VAT,Status,Invoice'
const row = (start: string, site: string, kwh: string, invoice: string, total: string, extra: Partial<{ vin: string; description: string; status: string }> = {}) =>
  `${start},Test Owner,${extra.vin ?? '5YJTEST0000000001'},my,US,"${site}",${extra.description ?? 'CHARGING : PAYMENT'},${kwh},N/A,N/A,N/A,N/A,${invoice},0.30/kwh,N/A,N/A,N/A,N/A,0,${total},${total},${extra.status ?? 'PAID'},https://tesla.com/teslaaccount/charging/invoice/${invoice}`
const csv = (...rows: string[]): string => [header, ...rows].join('\r\n')

const charge = (id: number, time: string, extra: Partial<ChargeRow> = {}): ChargeRow => ({
  id, time, location: 'Tesla Supercharger', type: 'DC', startBattery: 20, endBattery: 80,
  energyAddedKwh: 30, energyUsedKwh: 32, maxPowerKw: 150, cost: null, durationMin: 30,
  costPerKwh: null, efficiencyPercent: null, outsideTempC: 20, ratedRangeAddedKm: 200,
  idealRangeAddedKm: null, odometerKm: 10_000, ...extra,
})

describe('Supercharger CSV parsing', () => {
  it('parses quoted fields and CRLF rows', () => {
    expect(parseCsv('a,"b, c","d ""q"""\r\n1,2,3\r\n')).toEqual([['a', 'b, c', 'd "q"'], ['1', '2', '3']])
  })

  it('reads amounts in the formats an export may use', () => {
    expect(parseAmount('38.0800 kwh')).toBe(38.08)
    expect(parseAmount('$9.90')).toBe(9.9)
    expect(parseAmount('9,90 €')).toBe(9.9)
    expect(parseAmount('1,234.50')).toBe(1234.5)
    expect(parseAmount('-2.00')).toBe(-2)
    expect(parseAmount('N/A')).toBeNull()
  })

  it('reads instants with their offset and sums fee rows per invoice', () => {
    const parsed = parseSuperchargerCsv(csv(
      row('2026-10-13T21:55:50-04:00', 'New Albany, IN', '41.8170 kwh', 'INV-2', '15.47'),
      row('2026-10-13T21:55:50-04:00', 'New Albany, IN', '4 min', 'INV-2', '2', { description: 'PARKING : PAYMENT' }),
    ))
    const [session] = parsed.sessions
    expect(session?.start.toISOString()).toBe('2026-10-14T01:55:50.000Z')
    expect(session?.total).toBe(17.47)
    expect(session?.energyKwh).toBe(41.817)
    expect(session?.fees).toEqual(['CHARGING : PAYMENT', 'PARKING : PAYMENT'])
  })

  it('finds columns by name, so reordered or renamed columns still work', () => {
    const parsed = parseSuperchargerCsv('total inc vat,Site Location Name,Charge Start Date Time\n9.90,Somewhere,2026-10-14T05:02:13Z\n')
    expect(parsed.sessions).toHaveLength(1)
    expect(parsed.sessions[0]?.total).toBe(9.9)
    expect(parsed.notes.some((note) => note.includes('QuantityBase'))).toBe(true)
  })

  it('explains a file that is not the Supercharger export', () => {
    expect(() => parseSuperchargerCsv('date,amount\n2026-01-01,4')).toThrow(
      /Missing columns: ChargeStartDateTime, SiteLocationName, Total Inc\. VAT\. Columns found: date, amount/u,
    )
    expect(() => parseSuperchargerCsv('')).toThrow(/empty/u)
  })

  it('reports bad rows by line number and counts repeated rows once', () => {
    const good = row('2026-10-14T00:02:13-05:00', 'Nashville, TN', '38.08 kwh', 'INV-1', '9.9')
    const parsed = parseSuperchargerCsv(csv(
      good,
      good,
      row('not a date', 'X', '1 kwh', 'INV-9', '1'),
      row('2026-10-14T00:02:13-05:00', 'Y', '1 kwh', 'INV-8', 'N/A'),
    ))
    expect(parsed.sessions).toHaveLength(1)
    expect(parsed.duplicateRows).toBe(1)
    expect(parsed.problems).toEqual([
      { row: 4, message: 'unreadable start time "not a date"; skipped' },
      { row: 5, message: 'no total amount ("N/A"); skipped' },
    ])
  })
})

describe('Supercharger import plan (dry run)', () => {
  const file = csv(
    row('2026-10-14T00:02:13-05:00', 'Nashville, TN', '38.08 kwh', 'INV-1', '9.90'),
    row('2026-10-13T21:55:50-04:00', 'New Albany, IN', '30 kwh', 'INV-2', '15.47'),
    row('2026-10-12T20:00:00-05:00', 'Munster, IN', '30 kwh', 'INV-3', '2.97'),
    // Tesla billed one plug-in as two sessions, 23 minutes apart
    row('2026-10-02T01:53:04-05:00', 'Bowling Green, KY', '8.8 kwh', 'INV-4', '2.11'),
    row('2026-10-02T02:16:45-05:00', 'Bowling Green, KY', '22.8 kwh', 'INV-5', '5.46'),
    // never logged by teslalog
    row('2026-10-05T21:08:28-05:00', 'Nashville, TN', '25 kwh', 'INV-6', '9.06'),
  )
  const charges = [
    charge(1, '2026-10-14T05:03:40Z', { energyUsedKwh: 39 }), // INV-1, no cost yet
    charge(2, '2026-10-14T01:58:10Z', { cost: 15.47 }), // INV-2, same cost
    charge(3, '2026-10-13T01:01:00Z', { cost: 4 }), // INV-3, different cost
    charge(4, '2026-10-02T06:52:00Z', { durationMin: 50, energyUsedKwh: 31 }), // both INV-4 and INV-5
  ]
  const plan = planImport(parseSuperchargerCsv(file), charges)
  const item = (chargeId: number) => plan.items.find((entry) => entry.charge?.id === chargeId)

  it('classifies every session as new, same, conflict or skipped', () => {
    expect(item(1)?.status).toBe('new')
    expect(item(2)?.status).toBe('same')
    expect(item(3)?.status).toBe('conflict')
    const skipped = plan.items.filter((entry) => entry.status === 'skipped')
    expect(skipped).toHaveLength(1)
    expect(skipped[0]?.reason).toMatch(/no charge at this time/u)
  })

  it('combines sessions Tesla split from one plug-in', () => {
    expect(item(4)?.sessions).toHaveLength(2)
    expect(item(4)?.teslaTotal).toBe(7.57)
    expect(item(4)?.warnings[0]).toMatch(/2 Tesla sessions combined/u)
  })

  it('applies the chosen treatment for existing costs', () => {
    expect(resultingCost(item(1)!, 'keep')).toBe(9.9) // new: always the Tesla price
    expect(resultingCost(item(2)!, 'replace')).toBeNull() // already equal: untouched
    expect(resultingCost(item(3)!, 'replace')).toBe(2.97)
    expect(resultingCost(item(3)!, 'add')).toBe(6.97)
    expect(resultingCost(item(3)!, 'keep')).toBeNull()
  })

  it('warns about AC charges, kWh mismatches and unpaid sessions', () => {
    const warned = planImport(
      parseSuperchargerCsv(csv(row('2026-10-14T00:02:13-05:00', 'Nashville, TN', '80 kwh', 'INV-1', '9.90', { status: 'PENDING' }))),
      [charge(1, '2026-10-14T05:03:40Z', { type: 'AC', energyUsedKwh: 20, energyAddedKwh: 18 })],
    ).items[0]
    expect(warned?.warnings).toEqual([
      'teslalog recorded this charge as AC, not a Supercharger',
      'Tesla billed 80.0 kWh but the car recorded 20.0 kWh',
      'Tesla status: PENDING',
    ])
  })

  it('imports one car from a multi-car export and says why the rest were skipped', () => {
    const twoCars = parseSuperchargerCsv(csv(
      row('2026-10-14T00:02:13-05:00', 'A', '30 kwh', 'INV-1', '9.90'),
      row('2026-10-13T00:02:13-05:00', 'B', '30 kwh', 'INV-2', '8.00'),
      row('2026-10-12T00:02:13-05:00', 'C', '30 kwh', 'INV-3', '7.00', { vin: '5YJOTHER00000ABCDE' }),
    ))
    const multi = planImport(twoCars, [charge(1, '2026-10-14T05:03:00Z')])
    expect(multi.vins).toHaveLength(2)
    expect(multi.vin).toBe('5YJTEST0000000001')
    expect(multi.items.find((entry) => entry.sessions[0]?.site === 'C')?.reason).toBe('Another car (VIN …0ABCDE)')
  })

  it('writes one atomic statement, including undo back to no cost', () => {
    expect(costUpdateSql([{ chargeId: 1, cost: 9.9 }, { chargeId: 3, cost: null }])).toBe(
      'UPDATE charging_sessions SET cost = CASE id WHEN 1 THEN 9.9 WHEN 3 THEN CAST(NULL AS REAL) END WHERE id IN (1,3)',
    )
    expect(() => costUpdateSql([])).toThrow()
    expect(() => costUpdateSql([{ chargeId: 1, cost: -1 }])).toThrow()
  })
})
