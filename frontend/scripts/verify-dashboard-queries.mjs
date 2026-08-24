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
const firstCharge =
  database.exec("SELECT COALESCE(MIN(id), 0) FROM charging_sessions WHERE status = 'closed'")[0]
    ?.values[0]?.[0] ?? 0
// Every variable is exercised in both of its settings, because a column
// name only exists under one of them: $start_range expands to
// start_range_km or start_ideal_range_km, and a typo in either is a
// runtime error the other spelling would hide.
const variants = [
  { length: 'km', temp: 'C', range: 'rated', start: 'start_range_km', end: 'end_range_km', pos: 'range_km' },
  { length: 'mi', temp: 'F', range: 'ideal', start: 'start_ideal_range_km', end: 'end_ideal_range_km', pos: 'ideal_range_km' },
]
const substitute = (query, variant) =>
  query
    .replaceAll('$drive_id', String(firstDrive))
    .replaceAll('$charging_session_id', String(firstCharge))
    .replaceAll('$min_idle_hours', '1')
    .replaceAll('$min_distance', '1')
    .replaceAll('$period', 'month')
    .replaceAll('$length_unit', variant.length)
    .replaceAll('$temp_unit', variant.temp)
    .replaceAll('$start_range', variant.start)
    .replaceAll('$end_range', variant.end)
    .replaceAll('$pos_range', variant.pos)
    .replaceAll('$preferred_range', variant.range)

const failures = []
let verified = 0
for (const dashboard of catalog) {
  for (const panel of dashboard.panels) {
    for (const query of panel.queries) {
      for (const variant of variants) {
        try {
          database.exec(substitute(query, variant))
          verified += 1
        } catch (error) {
          failures.push(
            `${dashboard.key} / ${panel.title} [${variant.length}/${variant.range}]: ${error instanceof Error ? error.message : String(error)}`,
          )
        }
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
