export type LengthUnit = 'km' | 'mi'
export type TemperatureUnit = 'C' | 'F'
export type TimeRange = '24h' | '7d' | '30d' | '90d' | '1y' | 'all'
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

export const timeRangeSql = (range: TimeRange, column: string): string => {
  switch (range) {
    case '24h':
      return `${column} >= datetime('now', '-24 hours')`
    case '7d':
      return `${column} >= datetime('now', '-7 days')`
    case '30d':
      return `${column} >= datetime('now', '-30 days')`
    case '90d':
      return `${column} >= datetime('now', '-90 days')`
    case '1y':
      return `${column} >= datetime('now', '-1 year')`
    case 'all':
      return '1 = 1'
  }
}
