import { describe, expect, it } from 'vitest'
import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { columnMigrations, schemaStatements, schemaTables } from './generated/schema'

const generatedPath = resolve(import.meta.dirname, 'generated/schema.ts')

describe('generated schema', () => {
  // The importer used to hand-copy this schema, and it drifted: the whole
  // geocode_cache table went missing - so imported databases carried no
  // address data and the Locations dashboard failed on them - along with
  // two positions columns added later. Nothing detected it.
  //
  // Regenerating and comparing means a change to the Go schema that is
  // not synced fails here rather than silently producing a degraded
  // import months later.
  it('is in sync with internal/storage', () => {
    const before = readFileSync(generatedPath, 'utf8')
    execFileSync('node', [resolve(import.meta.dirname, '../scripts/sync-schema.mjs')], {
      stdio: 'pipe',
    })
    const after = readFileSync(generatedPath, 'utf8')
    expect(after, 'run `npm run sync:schema` and commit the result').toBe(before)
  })

  // Guards against a generator regression that silently emits nothing:
  // an empty schema would leave the importer building a database with no
  // tables, which fails far from the cause.
  it('covers every table the viewer requires', () => {
    for (const table of [
      'vehicles',
      'states',
      'drives',
      'positions',
      'charging_sessions',
      'charging_samples',
      'battery_samples',
      'software_updates',
      'geocode_cache',
    ]) {
      expect(schemaTables).toContain(table)
    }
  })

  it('carries the indexes, not just the tables', () => {
    expect(schemaStatements.filter((s) => s.startsWith('CREATE INDEX')).length).toBeGreaterThan(5)
  })

  // PRAGMA statements are the daemon's concern. Left in, foreign_keys=ON
  // would reject rows the importer inserts in dump order.
  it('excludes PRAGMA statements', () => {
    expect(schemaStatements.filter((s) => /^PRAGMA/i.test(s))).toEqual([])
  })

  it('includes the post-release column migrations', () => {
    expect(columnMigrations.length).toBeGreaterThan(10)
    expect(columnMigrations.every((m) => m.startsWith('ALTER TABLE '))).toBe(true)
    // The columns whose absence caused the original drift.
    expect(columnMigrations.some((m) => m.includes('battery_heater'))).toBe(true)
    expect(columnMigrations.some((m) => m.includes('geocode_cache'))).toBe(true)
  })
})
