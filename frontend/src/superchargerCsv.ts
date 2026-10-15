import type { ChargeRow } from './domain'
import { timestampDate } from './viewSettings'

// Import of Tesla's own Supercharger history CSV (tesla.com account or the
// Tesla app: Charging > Supercharger > download). The car's telemetry never
// carries what a session was billed - only Tesla's account history does -
// so this file is how real Supercharger prices reach teslalog.
//
// Everything here is a dry run: planImport reads the file and describes
// what would change, and nothing is written until the user has reviewed
// that plan and chosen how to treat charges that already have a cost.
//
// Tesla's export, October 2026: ChargeStartDateTime, Name, Vin, Model,
// Country, SiteLocationName, Description, QuantityBase, QuantityTier1-4,
// InvoiceNumber, UnitCostBase, UnitCostTier1-4, VAT, Total Exc. VAT,
// Total Inc. VAT, Status, Invoice. One row per fee: a session can carry a
// CHARGING row plus e.g. an idle fee under the same invoice number.
// Columns are found by name, so reordered or extra columns are fine, and
// each field accepts a few plausible renames in case the export changes -
// but not generic names like "date" or "amount", so an unrelated CSV is
// refused rather than half-matched.

export type SuperchargerSession = Readonly<{
  key: string
  start: Date
  site: string
  vin: string | null
  energyKwh: number | null
  total: number
  statuses: readonly string[]
  invoiceUrl: string | null
  fees: readonly string[]
  rows: readonly number[]
}>

export type RowProblem = Readonly<{ row: number; message: string }>

export type PlanStatus = 'new' | 'same' | 'conflict' | 'skipped'
export type PlanItem = Readonly<{
  key: string
  sessions: readonly SuperchargerSession[]
  charge: ChargeRow | null
  teslaTotal: number
  status: PlanStatus
  reason: string | null
  warnings: readonly string[]
}>

export type ImportPlan = Readonly<{
  items: readonly PlanItem[]
  problems: readonly RowProblem[]
  notes: readonly string[]
  vins: readonly string[]
  vin: string | null
  duplicateRows: number
}>

// What to do with a charge that already has a different cost.
export type ConflictChoice = 'replace' | 'add' | 'keep'

// ---------------------------------------------------------------- parsing

// RFC 4180 CSV: quoted fields may contain commas, newlines and "" escapes.
export const parseCsv = (text: string): string[][] => {
  const rows: string[][] = []
  let row: string[] = []
  let field = ''
  let quoted = false
  const source = text.replace(/^﻿/u, '')
  for (let index = 0; index < source.length; index += 1) {
    const char = source[index]
    if (quoted) {
      if (char === '"' && source[index + 1] === '"') {
        field += '"'
        index += 1
      } else if (char === '"') quoted = false
      else field += char
    } else if (char === '"') quoted = true
    else if (char === ',') {
      row.push(field)
      field = ''
    } else if (char === '\n' || char === '\r') {
      if (char === '\r' && source[index + 1] === '\n') index += 1
      row.push(field)
      rows.push(row)
      row = []
      field = ''
    } else field += char
  }
  if (field !== '' || row.length > 0) {
    row.push(field)
    rows.push(row)
  }
  return rows
}

const normalise = (name: string): string => name.toLowerCase().replace(/[^a-z0-9]/gu, '')

// Accepted header names per field, normalised. The first is Tesla's.
const columns = {
  start: ['chargestartdatetime', 'startdatetime', 'chargestart', 'starttime', 'startdate'],
  site: ['sitelocationname', 'sitename', 'site', 'location', 'locationname'],
  total: ['totalincvat', 'totalinclvat', 'totalincludingvat', 'totalinctax', 'total', 'totalamount'],
  description: ['description', 'feetype'],
  quantity: ['quantitybase', 'quantity', 'energy', 'energykwh', 'kwh'],
  invoiceNumber: ['invoicenumber', 'invoiceno', 'invoiceid'],
  invoiceUrl: ['invoice', 'invoiceurl', 'invoicelink'],
  status: ['status', 'paymentstatus'],
  vin: ['vin'],
} as const
type Field = keyof typeof columns
const requiredFields: readonly Field[] = ['start', 'site', 'total']
const fieldLabels: Readonly<Record<Field, string>> = {
  start: 'ChargeStartDateTime',
  site: 'SiteLocationName',
  total: 'Total Inc. VAT',
  description: 'Description',
  quantity: 'QuantityBase',
  invoiceNumber: 'InvoiceNumber',
  invoiceUrl: 'Invoice',
  status: 'Status',
  vin: 'Vin',
}

// A money or quantity cell: "9.9", "$9.90", "38.0800 kwh", or a decimal
// comma ("9,90") from a European locale. Null when there is no number.
export const parseAmount = (value: string | undefined): number | null => {
  let text = (value ?? '').trim()
  if (text === '' || /^n\/?a$/iu.test(text)) return null
  // "9,90" is a decimal comma; "1,234" (three digits) a thousands separator.
  if (!text.includes('.')) text = text.replace(/(\d),(\d{1,2})(?!\d)/u, '$1.$2')
  const match = /\d+(?:\.\d+)?/u.exec(text.replaceAll(',', ''))
  if (!match) return null
  const negative = /-/u.test(text.slice(0, text.search(/\d/u)))
  return negative ? -Number(match[0]) : Number(match[0])
}

// Tesla writes an explicit offset ("2026-10-14T00:02:13-05:00"), which
// fixes the instant. A time without one is read as this browser's local
// time and flagged, since it could be off by the time-zone difference.
const parseStart = (value: string): Readonly<{ date: Date; zoned: boolean }> | null => {
  const text = value.trim()
  if (!/^\d{4}-\d{2}-\d{2}/u.test(text)) return null
  const date = new Date(text.includes('T') || text.includes(' ') ? text.replace(' ', 'T') : text)
  if (Number.isNaN(date.getTime())) return null
  return { date, zoned: /(?:Z|[+-]\d{2}:?\d{2})$/iu.test(text) }
}

export type ParsedCsv = Readonly<{
  sessions: readonly SuperchargerSession[]
  problems: readonly RowProblem[]
  notes: readonly string[]
  duplicateRows: number
}>

export const parseSuperchargerCsv = (text: string): ParsedCsv => {
  const table = parseCsv(text)
  const headerIndex = table.findIndex((row) => row.some((cell) => cell.trim() !== ''))
  if (headerIndex < 0) throw new Error('The file is empty.')
  const header = table[headerIndex] ?? []
  const normalisedHeader = header.map(normalise)
  const index = Object.fromEntries(
    (Object.keys(columns) as Field[]).map((field) => [
      field,
      columns[field].map((alias) => normalisedHeader.indexOf(alias)).find((position) => position >= 0) ?? -1,
    ]),
  ) as Record<Field, number>
  const missing = requiredFields.filter((field) => index[field] < 0)
  if (missing.length > 0)
    throw new Error(
      `This does not look like Tesla's Supercharger history CSV. Missing column${missing.length === 1 ? '' : 's'}: ${missing
        .map((field) => fieldLabels[field])
        .join(', ')}. Columns found: ${header.map((name) => name.trim()).filter(Boolean).join(', ') || 'none'}.`,
    )
  const notes: string[] = []
  for (const field of ['description', 'quantity', 'invoiceNumber', 'status'] as const)
    if (index[field] < 0)
      notes.push(`No ${fieldLabels[field]} column; ${field === 'quantity' ? 'kWh checks are skipped' : field === 'invoiceNumber' ? 'fee rows are grouped by start time and site instead' : field === 'status' ? 'payment status is not checked' : 'all rows are treated as charging fees'}.`)

  const problems: RowProblem[] = []
  const seen = new Set<string>()
  let duplicateRows = 0
  let unzoned = 0
  const grouped = new Map<string, { session: Omit<SuperchargerSession, 'statuses' | 'fees' | 'rows'>; statuses: string[]; fees: string[]; rows: number[] }>()
  table.slice(headerIndex + 1).forEach((cells, offset) => {
    const rowNumber = headerIndex + offset + 2
    if (cells.every((cell) => cell.trim() === '')) return
    const get = (field: Field): string => (index[field] >= 0 ? (cells[index[field]] ?? '').trim() : '')
    const signature = cells.map((cell) => cell.trim()).join('\u0000')
    if (seen.has(signature)) {
      duplicateRows += 1
      return
    }
    seen.add(signature)
    if (cells.length < header.length - 1) {
      problems.push({ row: rowNumber, message: `has ${cells.length} cells but the header has ${header.length}; skipped` })
      return
    }
    const start = parseStart(get('start'))
    if (!start) {
      problems.push({ row: rowNumber, message: `unreadable start time "${get('start')}"; skipped` })
      return
    }
    const total = parseAmount(get('total'))
    if (total === null) {
      problems.push({ row: rowNumber, message: `no total amount ("${get('total')}"); skipped` })
      return
    }
    if (!start.zoned) unzoned += 1
    const site = get('site') || 'Unknown Supercharger'
    const invoice = get('invoiceNumber')
    const key = invoice && !/^n\/?a$/iu.test(invoice) ? invoice : `${start.date.toISOString()}|${site}`
    const description = get('description') || 'CHARGING'
    const isCharging = /charg/iu.test(description) || index.description < 0
    const quantity = get('quantity')
    const energy = isCharging && (/kwh/iu.test(quantity) || index.quantity >= 0) ? parseAmount(quantity) : null
    const existing = grouped.get(key)
    if (existing) {
      existing.session = {
        ...existing.session,
        total: existing.session.total + total,
        energyKwh: existing.session.energyKwh ?? energy,
      }
      existing.fees.push(description)
      existing.statuses.push(get('status'))
      existing.rows.push(rowNumber)
    } else {
      grouped.set(key, {
        session: {
          key,
          start: start.date,
          site,
          vin: get('vin') || null,
          energyKwh: energy,
          total,
          invoiceUrl: /^https:\/\//iu.test(get('invoiceUrl')) ? get('invoiceUrl') : null,
        },
        statuses: [get('status')],
        fees: [description],
        rows: [rowNumber],
      })
    }
  })
  if (unzoned > 0)
    notes.push(`${unzoned} row${unzoned === 1 ? ' has' : 's have'} no time-zone offset and were read as this browser's local time.`)
  const sessions = [...grouped.values()]
    .map(({ session, statuses, fees, rows }) => ({
      ...session,
      total: Math.round(session.total * 100) / 100,
      statuses: [...new Set(statuses.filter(Boolean))],
      fees,
      rows,
    }))
    .toSorted((a, b) => b.start.getTime() - a.start.getTime())
  return { sessions, problems, notes, duplicateRows }
}

// --------------------------------------------------------------- matching

const minute = 60_000
const before = 15 * minute
const after = 5 * minute

const chargeSpan = (charge: ChargeRow): Readonly<{ start: number; end: number }> => {
  const start = timestampDate(charge.time).getTime()
  return { start, end: start + Math.max(charge.durationMin ?? 0, 0) * minute }
}

// The matching rule: a Tesla session belongs to the teslalog charge whose
// time span contains the session's start, widened by 15 minutes before
// (the car can report the start a little late after plugging in) and 5
// after. When several charges qualify, DC beats AC, then the nearest one.
// Tesla can bill one plug-in as several sessions; those all land on the
// same charge and their prices are added together.
const bestCharge = (session: SuperchargerSession, charges: readonly ChargeRow[]) => {
  const at = session.start.getTime()
  const candidates = charges.flatMap((charge) => {
    const span = chargeSpan(charge)
    if (at < span.start - before || at > span.end + after) return []
    const distance = at < span.start ? span.start - at : at > span.end ? at - span.end : 0
    return [{ charge, distance }]
  })
  candidates.sort(
    (a, b) => Number(a.charge.type !== 'DC') - Number(b.charge.type !== 'DC') || a.distance - b.distance,
  )
  return candidates[0]?.charge ?? null
}

const sameCost = (a: number, b: number): boolean => Math.abs(a - b) < 0.005

export const planImport = (
  parsed: ParsedCsv,
  charges: readonly ChargeRow[],
  vin: string | null = null,
): ImportPlan => {
  const vins = [...new Set(parsed.sessions.flatMap((session) => (session.vin ? [session.vin] : [])))]
  // Several cars in one export: default to the one with the most sessions.
  const chosenVin =
    vin ??
    (vins.length > 1
      ? (vins
          .map((candidate) => [candidate, parsed.sessions.filter((session) => session.vin === candidate).length] as const)
          .toSorted((a, b) => b[1] - a[1])[0]?.[0] ?? null)
      : null)
  const oldest = charges.reduce(
    (earliest, charge) => Math.min(earliest, timestampDate(charge.time).getTime()),
    Number.POSITIVE_INFINITY,
  )
  const skipped: PlanItem[] = []
  const byCharge = new Map<number, { charge: ChargeRow; sessions: SuperchargerSession[] }>()
  for (const session of parsed.sessions) {
    const skip = (reason: string): void => {
      skipped.push({ key: session.key, sessions: [session], charge: null, teslaTotal: session.total, status: 'skipped', reason, warnings: [] })
    }
    if (chosenVin && session.vin && session.vin !== chosenVin) {
      skip(`Another car (VIN …${session.vin.slice(-6)})`)
      continue
    }
    if (session.total < 0) {
      skip('Negative total (a refund or credit); left for you to handle')
      continue
    }
    const charge = bestCharge(session, charges)
    if (!charge) {
      skip(
        session.start.getTime() < oldest - before
          ? 'Older than the charges loaded in teslalog'
          : 'teslalog has no charge at this time (not tracked, or the logger was offline)',
      )
      continue
    }
    const entry = byCharge.get(charge.id)
    if (entry) entry.sessions.push(session)
    else byCharge.set(charge.id, { charge, sessions: [session] })
  }

  const matched: PlanItem[] = [...byCharge.values()].map(({ charge, sessions }) => {
    const teslaTotal = Math.round(sessions.reduce((sum, session) => sum + session.total, 0) * 100) / 100
    const warnings: string[] = []
    if (sessions.length > 1) warnings.push(`${sessions.length} Tesla sessions combined into this one charge`)
    if (charge.type !== 'DC') warnings.push('teslalog recorded this charge as AC, not a Supercharger')
    const teslaKwh = sessions.reduce((sum, session) => sum + (session.energyKwh ?? 0), 0)
    const carKwh = Math.max(charge.energyUsedKwh ?? 0, charge.energyAddedKwh ?? 0)
    if (teslaKwh > 0 && carKwh > 0 && (teslaKwh > carKwh * 1.5 + 2 || teslaKwh < carKwh / 1.5 - 2))
      warnings.push(`Tesla billed ${teslaKwh.toFixed(1)} kWh but the car recorded ${carKwh.toFixed(1)} kWh`)
    const statuses = [...new Set(sessions.flatMap((session) => session.statuses))]
    const unpaid = statuses.filter((status) => !/^paid$/iu.test(status))
    if (unpaid.length > 0) warnings.push(`Tesla status: ${unpaid.join(', ')}`)
    const status: PlanStatus =
      charge.cost === null ? 'new' : sameCost(charge.cost, teslaTotal) ? 'same' : 'conflict'
    return { key: `charge-${charge.id}`, sessions, charge, teslaTotal, status, reason: null, warnings }
  })

  return {
    items: [...matched, ...skipped].toSorted(
      (a, b) => (b.sessions[0]?.start.getTime() ?? 0) - (a.sessions[0]?.start.getTime() ?? 0),
    ),
    problems: parsed.problems,
    notes: parsed.notes,
    vins,
    vin: chosenVin,
    duplicateRows: parsed.duplicateRows,
  }
}

// ---------------------------------------------------------------- applying

// The cost an item would end with for a given choice; null means the
// item is left unchanged.
export const resultingCost = (item: PlanItem, choice: ConflictChoice): number | null => {
  if (!item.charge || item.status === 'skipped' || item.status === 'same') return null
  if (item.status === 'new') return item.teslaTotal
  if (choice === 'keep') return null
  const existing = item.charge.cost ?? 0
  return Math.round((choice === 'add' ? existing + item.teslaTotal : item.teslaTotal) * 100) / 100
}

export type CostUpdate = Readonly<{ chargeId: number; cost: number | null }>

// One statement for the whole import, so it applies completely or not at
// all. Ids and costs are numbers produced above, never text from the file;
// a null cost (used by undo) restores "no cost".
export const costUpdateSql = (updates: readonly CostUpdate[]): string => {
  const valid = updates.filter(
    (update) =>
      Number.isInteger(update.chargeId) &&
      (update.cost === null || (Number.isFinite(update.cost) && update.cost >= 0)),
  )
  if (valid.length === 0) throw new Error('Nothing to import.')
  if (valid.length !== updates.length) throw new Error('Refusing to write an invalid cost.')
  const cases = valid
    // A typed NULL: PostgreSQL rejects an untyped one assigned to a REAL.
    .map((update) => `WHEN ${update.chargeId} THEN ${update.cost === null ? 'CAST(NULL AS REAL)' : update.cost}`)
    .join(' ')
  const ids = valid.map((update) => update.chargeId).join(',')
  return `UPDATE charging_sessions SET cost = CASE id ${cases} END WHERE id IN (${ids})`
}
