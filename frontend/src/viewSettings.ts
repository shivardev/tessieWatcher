export type LengthUnit = 'km' | 'mi'
export type TemperatureUnit = 'C' | 'F'
type RelativeUnit = 's' | 'm' | 'h' | 'd' | 'w' | 'M' | 'y'
// Grafana-style quick ranges. Each is relative to the moment a query
// runs, so it reaches SQLite as a datetime('now', modifier) bound rather
// than a frozen instant.
export const quickRanges = [
  { key: '5m', label: 'Last 5 minutes', amount: 5, unit: 'm' },
  { key: '15m', label: 'Last 15 minutes', amount: 15, unit: 'm' },
  { key: '30m', label: 'Last 30 minutes', amount: 30, unit: 'm' },
  { key: '1h', label: 'Last 1 hour', amount: 1, unit: 'h' },
  { key: '3h', label: 'Last 3 hours', amount: 3, unit: 'h' },
  { key: '6h', label: 'Last 6 hours', amount: 6, unit: 'h' },
  { key: '12h', label: 'Last 12 hours', amount: 12, unit: 'h' },
  { key: '24h', label: 'Last 24 hours', amount: 24, unit: 'h' },
  { key: '2d', label: 'Last 2 days', amount: 2, unit: 'd' },
  { key: '7d', label: 'Last 7 days', amount: 7, unit: 'd' },
  { key: '30d', label: 'Last 30 days', amount: 30, unit: 'd' },
  { key: '90d', label: 'Last 90 days', amount: 90, unit: 'd' },
  { key: '6M', label: 'Last 6 months', amount: 6, unit: 'M' },
  { key: '1y', label: 'Last 1 year', amount: 1, unit: 'y' },
  { key: '2y', label: 'Last 2 years', amount: 2, unit: 'y' },
  { key: '5y', label: 'Last 5 years', amount: 5, unit: 'y' },
] as const satisfies readonly Readonly<{
  key: string
  label: string
  amount: number
  unit: RelativeUnit
}>[]
export type QuickRange = (typeof quickRanges)[number]['key']
export type TimeRange = QuickRange | 'all' | 'custom'
// Which range figure the car's own displays are set to. Tesla reports
// both: rated range follows the EPA/WLTP rating, ideal range the
// manufacturer's best case. TeslaMate exposes the same choice as its
// $preferred_range variable and every efficiency figure depends on it,
// so a viewer fixed to one of them disagrees with the car's dash for
// everyone set to the other.
export type PreferredRange = 'rated' | 'ideal'
export type StatisticsPeriod = 'day' | 'week' | 'month' | 'year'

export type ViewSettings = Readonly<{
  lengthUnit: LengthUnit
  temperatureUnit: TemperatureUnit
  timeRange: TimeRange
  // Used only when timeRange is 'custom'. Each side is a Grafana-style
  // expression: an absolute local "YYYY-MM-DD HH:mm:ss" (or bare date),
  // "now", or "now-7d". Both ends are inclusive; an empty side is open.
  customFrom: string
  customTo: string
  preferredRange: PreferredRange
  // Drives shorter than this are excluded from efficiency aggregates.
  // A 300 m crawl out of the garage has a real distance and a rounded
  // battery delta, so its implied consumption is nonsense that would
  // otherwise dominate a temperature bucket. TeslaMate's $min_distance.
  minDistance: number
  statisticsPeriod: StatisticsPeriod
}>

export const defaultViewSettings: ViewSettings = {
  lengthUnit: 'mi',
  temperatureUnit: 'F',
  timeRange: '90d',
  customFrom: '',
  customTo: '',
  preferredRange: 'rated',
  minDistance: 1,
  statisticsPeriod: 'month',
}

export const kilometersToMiles = (kilometers: number): number => kilometers / 1.60934
export const kilometersPerHourToMilesPerHour = (speed: number): number => speed / 1.60934
export const celsiusToFahrenheit = (temperature: number): number => (temperature * 9) / 5 + 32

export const distance = (kilometers: number, unit: LengthUnit): number =>
  unit === 'mi' ? kilometersToMiles(kilometers) : kilometers

export const speed = (kilometersPerHour: number, unit: LengthUnit): number =>
  unit === 'mi' ? kilometersPerHourToMilesPerHour(kilometersPerHour) : kilometersPerHour

export const temperature = (celsius: number, unit: TemperatureUnit): number =>
  unit === 'F' ? celsiusToFahrenheit(celsius) : celsius

export const timestampDate = (value: string | number): Date => {
  if (typeof value === 'number') return new Date(value)
  const timestamp = /(?:Z|[+-]\d{2}(?::?\d{2})?)$/u.test(value)
    ? value
    : `${value.replace(' ', 'T')}Z`
  return new Date(timestamp)
}

export type TimeFilter = Readonly<{
  timeRange: TimeRange
  customFrom?: string
  customTo?: string
}>

const sqliteUnit: Readonly<Record<RelativeUnit, string>> = {
  s: 'seconds',
  m: 'minutes',
  h: 'hours',
  d: 'days',
  w: 'days',
  M: 'months',
  y: 'years',
}

const shift = (date: Date, amount: number, unit: RelativeUnit): Date => {
  const next = new Date(date)
  switch (unit) {
    case 's':
      next.setSeconds(next.getSeconds() + amount)
      break
    case 'm':
      next.setMinutes(next.getMinutes() + amount)
      break
    case 'h':
      next.setHours(next.getHours() + amount)
      break
    case 'd':
      next.setDate(next.getDate() + amount)
      break
    case 'w':
      next.setDate(next.getDate() + amount * 7)
      break
    case 'M':
      next.setMonth(next.getMonth() + amount)
      break
    case 'y':
      next.setFullYear(next.getFullYear() + amount)
      break
  }
  return next
}

const quickRange = (key: TimeRange) => quickRanges.find((range) => range.key === key)

const pad = (value: number): string => String(value).padStart(2, '0')

// formatTimeInput renders an instant the way the picker's text fields
// accept it back: local "YYYY-MM-DD HH:mm:ss".
export const formatTimeInput = (date: Date): string =>
  `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`

// parseTimeExpression resolves one side of a range to an instant, or null
// for anything it does not recognise. Custom values reach SQL, so only the
// resulting Date is ever used - never the raw text. A bare date as the end
// of a range means the end of that day, so "2026-09-01" to "2026-09-30"
// covers all of September.
export const parseTimeExpression = (
  value: string | undefined,
  side: 'from' | 'to',
  now: number = Date.now(),
): Date | null => {
  const text = (value ?? '').trim()
  const relative = /^now(?:\s*([+-])\s*(\d+)\s*([smhdwMy]))?$/u.exec(text)
  if (relative) {
    if (relative[1] === undefined) return new Date(now)
    const amount = Number(relative[2]) * (relative[1] === '-' ? -1 : 1)
    return shift(new Date(now), amount, relative[3] as RelativeUnit)
  }
  const absolute = /^(\d{4})-(\d{2})-(\d{2})(?:[ T](\d{1,2}):(\d{2})(?::(\d{2}))?)?$/u.exec(text)
  if (!absolute) return null
  const [year, month, day] = [Number(absolute[1]), Number(absolute[2]), Number(absolute[3])]
  const timeGiven = absolute[4] !== undefined
  const endOfDay = side === 'to' && !timeGiven
  const [hours, minutes, seconds] = timeGiven
    ? [Number(absolute[4]), Number(absolute[5]), Number(absolute[6] ?? 0)]
    : endOfDay
      ? [23, 59, 59]
      : [0, 0, 0]
  const date = new Date(year, month - 1, day, hours, minutes, seconds, endOfDay ? 999 : 0)
  // Reject rollovers such as 2026-02-31 or 25:00 rather than silently
  // landing on another day.
  if (
    date.getFullYear() !== year ||
    date.getMonth() !== month - 1 ||
    date.getDate() !== day ||
    date.getHours() !== hours ||
    date.getMinutes() !== minutes ||
    date.getSeconds() !== seconds
  )
    return null
  return date
}

// The From/To text a range shows in the picker: Grafana's "now-7d"/"now"
// for quick ranges, the stored expressions for a custom one.
export const timeRangeInputs = (filter: TimeFilter): Readonly<{ from: string; to: string }> => {
  if (filter.timeRange === 'custom')
    return { from: filter.customFrom ?? '', to: filter.customTo ?? '' }
  const range = quickRange(filter.timeRange)
  return range ? { from: `now-${range.amount}${range.unit}`, to: 'now' } : { from: '', to: '' }
}

// rangeFromInputs is the inverse: "now-7d"/"now" collapses back to the
// 7d quick range so it stays relative in SQL.
export const rangeFromInputs = (from: string, to: string): ViewSettingsTime => {
  const trimmedFrom = from.trim()
  const trimmedTo = to.trim()
  if (trimmedFrom === '' && trimmedTo === '')
    return { timeRange: 'all', customFrom: '', customTo: '' }
  if (trimmedTo === 'now') {
    const match = quickRanges.find((range) => `now-${range.amount}${range.unit}` === trimmedFrom)
    if (match) return { timeRange: match.key, customFrom: '', customTo: '' }
  }
  return { timeRange: 'custom', customFrom: trimmedFrom, customTo: trimmedTo }
}
export type ViewSettingsTime = Pick<ViewSettings, 'timeRange' | 'customFrom' | 'customTo'>

// timeWindow resolves a filter to a closed [from, to] instant range;
// null on either side means unbounded.
export const timeWindow = (
  filter: TimeFilter,
  now: number = Date.now(),
): Readonly<{ from: Date | null; to: Date | null }> => {
  if (filter.timeRange === 'all') return { from: null, to: null }
  if (filter.timeRange === 'custom')
    return {
      from: parseTimeExpression(filter.customFrom, 'from', now),
      to: parseTimeExpression(filter.customTo, 'to', now),
    }
  const range = quickRange(filter.timeRange)
  return { from: range ? shift(new Date(now), -range.amount, range.unit) : null, to: null }
}

export const withinTimeWindow = (value: string | number, filter: TimeFilter): boolean => {
  const { from, to } = timeWindow(filter)
  const time = timestampDate(value).getTime()
  return (from === null || time >= from.getTime()) && (to === null || time <= to.getTime())
}

// SQLite's own datetime() text form, in UTC, which is how timestamps are stored.
export const sqlDateTime = (date: Date): string =>
  date.toISOString().slice(0, 19).replace('T', ' ')

// quickRangeModifier is the datetime('now', ...) modifier for a quick range.
export const quickRangeModifier = (key: QuickRange): string => {
  const range = quickRange(key)
  if (!range) throw new Error(`Unknown quick range ${key}`)
  const unit = sqliteUnit[range.unit]
  return `-${range.amount} ${range.amount === 1 ? unit.replace(/s$/u, '') : unit}`
}

const customRangeSql = (filter: TimeFilter, column: string): string => {
  const { from, to } = timeWindow(filter)
  const bounds = [
    ...(from === null ? [] : [`${column} >= datetime('${sqlDateTime(from)}')`]),
    ...(to === null ? [] : [`${column} <= datetime('${sqlDateTime(to)}')`]),
  ]
  return bounds.length === 0 ? '1 = 1' : `(${bounds.join(' AND ')})`
}

export const timeRangeSql = (filter: TimeRange | TimeFilter, column: string): string => {
  const resolved = typeof filter === 'string' ? { timeRange: filter } : filter
  if (resolved.timeRange === 'all') return '1 = 1'
  if (resolved.timeRange === 'custom') return customRangeSql(resolved, column)
  return `${column} >= datetime('now', '${quickRangeModifier(resolved.timeRange)}')`
}

const sideLabel = (value: string | undefined, side: 'from' | 'to'): string | null => {
  const text = (value ?? '').trim()
  const date = parseTimeExpression(text, side)
  if (date === null) return null
  if (text.startsWith('now')) return text
  const midnight = date.getHours() === 0 && date.getMinutes() === 0 && date.getSeconds() === 0
  const endOfDay = date.getHours() === 23 && date.getMinutes() === 59 && date.getSeconds() === 59
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    ...(midnight || endOfDay ? {} : { timeStyle: 'short' }),
  }).format(date)
}

// Human label for the picker and page headings: "Last 7 days",
// "All time", "Sep 1, 2026 to Sep 30, 2026".
export const timeRangeLabel = (filter: TimeFilter): string => {
  if (filter.timeRange === 'all') return 'All time'
  if (filter.timeRange !== 'custom')
    return quickRange(filter.timeRange)?.label ?? filter.timeRange
  const from = sideLabel(filter.customFrom, 'from')
  const to = sideLabel(filter.customTo, 'to')
  if (from === null && to === null) return 'All time'
  if (from === null) return `Until ${to}`
  if (to === null || to === 'now') return `Since ${from}`
  return `${from} to ${to}`
}

const absoluteRange = (from: number, to: number, now: number): ViewSettingsTime => ({
  timeRange: 'custom',
  customFrom: formatTimeInput(new Date(from)),
  // A window that reaches the present keeps following it.
  customTo: to >= now - 1000 ? 'now' : formatTimeInput(new Date(to)),
})

// shiftTimeRange moves the window back (-1) or forward (+1) by its own
// length, like Grafana's arrow buttons. It never moves past now.
export const shiftTimeRange = (
  filter: TimeFilter,
  direction: -1 | 1,
  now: number = Date.now(),
): ViewSettingsTime | null => {
  const { from, to } = timeWindow(filter, now)
  if (from === null) return null
  const end = Math.min((to ?? new Date(now)).getTime(), now)
  const span = end - from.getTime()
  if (span <= 0) return null
  const nextTo = Math.min(end + direction * span, now)
  return absoluteRange(nextTo - span, nextTo, now)
}

// zoomOutTimeRange doubles the window around its centre, never past now.
export const zoomOutTimeRange = (
  filter: TimeFilter,
  now: number = Date.now(),
): ViewSettingsTime | null => {
  const { from, to } = timeWindow(filter, now)
  if (from === null) return null
  const end = Math.min((to ?? new Date(now)).getTime(), now)
  const span = end - from.getTime()
  if (span <= 0) return null
  const nextTo = Math.min(end + span / 2, now)
  return absoluteRange(nextTo - span * 2, nextTo, now)
}
