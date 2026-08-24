import { mkdir, readFile, readdir, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const scriptDirectory = dirname(fileURLToPath(import.meta.url))
const frontendDirectory = resolve(scriptDirectory, '..')
const grafanaDirectory = resolve(frontendDirectory, '..', 'grafana')
const outputPath = join(frontendDirectory, 'src', 'generated', 'dashboard-catalog.json')

const fileNames = (await readdir(grafanaDirectory))
  .filter((name) => name.startsWith('teslalog-') && name.endsWith('.json'))
  .sort()

const dashboards = []
for (const fileName of fileNames) {
  const source = JSON.parse(await readFile(join(grafanaDirectory, fileName), 'utf8'))
  dashboards.push({
    key: fileName.slice('teslalog-'.length, -'.json'.length),
    title: source.title,
    panels: (source.panels ?? []).map((panel) => ({
      id: panel.id,
      title: panel.title,
      type: panel.type,
      gridPos: panel.gridPos,
      options: panel.options ?? {},
      fieldConfig: panel.fieldConfig ?? {},
      queries: (panel.targets ?? []).map((target) => target.queryText ?? target.rawQueryText ?? ''),
    })),
  })
}

await mkdir(dirname(outputPath), { recursive: true })
await writeFile(outputPath, `${JSON.stringify(dashboards, null, 2)}\n`, 'utf8')
console.log(`Generated ${dashboards.length} dashboards at ${outputPath}`)
