// build-dashboard.mjs - turns a compact panel spec into a full Grafana
// dashboard JSON for grafana/teslalog-*.json.
//
// These files are the single source for two consumers: Grafana (via the
// frser-sqlite-datasource plugin) and the browser viewer (via
// sync-dashboard-catalog.mjs, which reads targets[].queryText). Hand-
// writing the Grafana boilerplate for every panel is where drift between
// the two crept in, so panels are declared as {type, title, w, h, sql}
// and the wrapper is generated.
import { writeFile } from 'node:fs/promises'

const datasource = { type: 'frser-sqlite-datasource', uid: '${DS_SQLITE}' }

const target = (sql) => ({
  datasource,
  queryText: sql,
  rawQueryText: sql,
  queryType: 'table',
  timeColumns: ['time', 'ts'],
  refId: 'A',
})

// Panels are laid out left to right, wrapping at 24 grid columns - the
// same flow Grafana's own auto-layout uses, so the JSON opens looking
// the way it was written rather than needing manual gridPos arithmetic.
const layout = (panels) => {
  let x = 0
  let y = 0
  let rowHeight = 0
  return panels.map((panel, index) => {
    const w = panel.w ?? 24
    const h = panel.h ?? 8
    if (x + w > 24) {
      x = 0
      y += rowHeight
      rowHeight = 0
    }
    const gridPos = { x, y, w, h }
    x += w
    rowHeight = Math.max(rowHeight, h)
    return { ...panel, id: index + 1, gridPos, w: undefined, h: undefined }
  })
}

const defaultOptions = (type) => {
  if (type === 'stat' || type === 'gauge')
    return { reduceOptions: { calcs: ['lastNotNull'] }, textMode: 'auto' }
  if (type === 'piechart') return { legend: { displayMode: 'list', placement: 'right' } }
  if (type === 'barchart' || type === 'bargauge') return { orientation: 'horizontal' }
  return {}
}

export const buildDashboard = async ({ uid, title, description, from = 'now-90d', panels }) => {
  const dashboard = {
    id: null,
    uid,
    title,
    description,
    tags: ['teslalog'],
    timezone: 'browser',
    schemaVersion: 39,
    version: 1,
    editable: true,
    graphTooltip: 1,
    time: { from, to: 'now' },
    templating: {
      list: [
        {
          current: { selected: false, text: 'SQLite', value: 'sqlite' },
          hide: 0,
          includeAll: false,
          label: 'Datasource',
          multi: false,
          name: 'DS_SQLITE',
          options: [],
          query: 'frser-sqlite-datasource',
          queryValue: '',
          refresh: 1,
          regex: '',
          skipUrlSync: false,
          type: 'datasource',
        },
      ],
    },
    panels: layout(panels).map(({ id, type, title: panelTitle, gridPos, sql, unit, decimals, options }) => ({
      id,
      type,
      title: panelTitle,
      datasource,
      gridPos,
      targets: [target(sql)],
      fieldConfig: { defaults: { unit: unit ?? 'none', ...(decimals === undefined ? {} : { decimals }) }, overrides: [] },
      options: options ?? defaultOptions(type),
    })),
  }
  const path = new URL(`../../grafana/teslalog-${uid.replace(/^teslalog-/u, '')}.json`, import.meta.url)
  await writeFile(path, `${JSON.stringify(dashboard, null, 2)}\n`, 'utf8')
  return path
}
