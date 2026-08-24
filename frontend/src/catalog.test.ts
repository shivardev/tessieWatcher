import { describe, expect, it } from 'vitest'
import { dashboardCatalog } from './catalog'
import { queryVariables } from './database'
import { groups } from './domain'
import { isDashboardImplemented } from './dashboardRegistry'

describe('dashboard inventory', () => {
  it('matches the 19 live TeslaMate navigation dashboards', () => {
    expect(groups.flatMap((group) => group.items)).toHaveLength(19)
    expect(groups.flatMap((group) => group.items).every(isDashboardImplemented)).toBe(true)
  })

  it('loads every canonical teslalog dashboard and query', () => {
    expect(dashboardCatalog).toHaveLength(16)
    // Every panel must carry at least one query: sync-dashboard-catalog
    // reads targets[].queryText, so a panel authored without one renders
    // as an empty box rather than failing loudly.
    const panels = dashboardCatalog.flatMap((dashboard) => dashboard.panels)
    expect(panels.every((panel) => panel.queries.length > 0)).toBe(true)
    expect(panels.every((panel) => panel.queries.every((query) => query.trim() !== ''))).toBe(true)
  })

  // A query referencing a $variable that interpolate does not know about
  // reaches SQLite verbatim and fails at runtime - on one dashboard, for
  // one user, with no build-time warning. Asserted against the live
  // substitution table rather than a hand-copied list so the two cannot
  // drift apart.
  it('contains only explicitly supported query variables', () => {
    const supported = new Set(Object.keys(queryVariables))
    const used = dashboardCatalog
      .flatMap((dashboard) => dashboard.panels)
      .flatMap((panel) => panel.queries)
      .flatMap((query) => query.match(/\$[a-z_]+/gu) ?? [])
    const unsupported = [...new Set(used)].filter((name) => !supported.has(name))
    expect(unsupported).toEqual([])
  })
})
