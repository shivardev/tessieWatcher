import { describe, expect, it } from 'vitest'
import { dashboardCatalog } from './catalog'
import { groups } from './domain'
import { isDashboardImplemented } from './dashboardRegistry'

describe('dashboard inventory', () => {
  it('matches the 19 live TeslaMate navigation dashboards', () => {
    expect(groups.flatMap((group) => group.items)).toHaveLength(19)
    expect(groups.flatMap((group) => group.items).every(isDashboardImplemented)).toBe(true)
  })

  it('loads every canonical teslalog dashboard and query', () => {
    expect(dashboardCatalog).toHaveLength(16)
    expect(
      dashboardCatalog.flatMap((dashboard) => dashboard.panels).flatMap((panel) => panel.queries),
    ).toHaveLength(58)
  })

  it('contains only explicitly supported query variables', () => {
    const variables = dashboardCatalog
      .flatMap((dashboard) => dashboard.panels)
      .flatMap((panel) => panel.queries)
      .flatMap((query) => query.match(/\$[a-z_]+/g) ?? [])
    expect(new Set(variables)).toEqual(
      new Set(['$drive_id', '$length_unit', '$temp_unit', '$min_idle_hours']),
    )
  })
})
