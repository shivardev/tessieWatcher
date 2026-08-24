import type { Dashboard } from './domain'

export const catalogDashboardKeys: Partial<Record<Dashboard, string>> = {
  'Charging stats': 'charging-stats',
  'Charge level': 'battery',
  Efficiency: 'efficiency',
  Locations: 'locations',
  Mileage: 'mileage',
  'Projected range': 'projected-range',
  States: 'states',
  Statistics: 'statistics',
  Timeline: 'timeline',
  Updates: 'updates',
  'Vampire drain': 'vampire-drain',
}

export const customDashboardNames: readonly Dashboard[] = [
  'Overview',
  'Drives',
  'Drive stats',
  'Charges',
  'Battery health',
  'Trip',
  'Visited',
  'Database information',
]

export const isDashboardImplemented = (dashboard: Dashboard): boolean =>
  customDashboardNames.includes(dashboard) || catalogDashboardKeys[dashboard] !== undefined
