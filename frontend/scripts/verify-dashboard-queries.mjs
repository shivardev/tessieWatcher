import initSqlJs from 'sql.js'
import { readFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const frontendDirectory = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const catalog = JSON.parse(
  await readFile(resolve(frontendDirectory, 'src/generated/dashboard-catalog.json'), 'utf8'),
)
const schemaSource = await readFile(
  resolve(frontendDirectory, '..', 'internal/storage/schema.go'),
  'utf8',
)
const schemaMatch = schemaSource.match(/const schema = `([\s\S]*?)`/)
if (!schemaMatch?.[1])
  throw new Error('Could not extract the canonical SQLite schema from internal/storage/schema.go')
const SQL = await initSqlJs()
const database = new SQL.Database()
database.exec(schemaMatch[1])
const firstDrive =
  database.exec("SELECT COALESCE(MIN(id), 0) FROM drives WHERE status = 'closed'")[0]
    ?.values[0]?.[0] ?? 0
const substitute = (query) =>
  query
    .replaceAll('$drive_id', String(firstDrive))
    .replaceAll('$min_idle_hours', '1')
    .replaceAll('$length_unit', 'km')
    .replaceAll('$temp_unit', 'C')

const failures = []
let verified = 0
for (const dashboard of catalog) {
  for (const panel of dashboard.panels) {
    for (const query of panel.queries) {
      try {
        database.exec(substitute(query))
        verified += 1
      } catch (error) {
        failures.push(
          `${dashboard.key} / ${panel.title}: ${error instanceof Error ? error.message : String(error)}`,
        )
      }
    }
  }
}
database.close()
if (failures.length > 0) {
  console.error(failures.join('\n'))
  process.exitCode = 1
} else {
  console.log(`Verified ${verified} dashboard queries against the canonical teslalog schema`)
}
