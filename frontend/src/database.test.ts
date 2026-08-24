import { readFile } from 'node:fs/promises'
import initSqlJs, { type Database } from 'sql.js'
import { beforeAll, describe, expect, it } from 'vitest'
import { openDatabase } from './database'

let schema = ''

beforeAll(async () => {
  const source = await readFile('../internal/storage/schema.go', 'utf8')
  const match = source.match(/const schema = `([\s\S]*?)`/)
  if (!match?.[1]) throw new Error('Canonical schema not found')
  schema = match[1]
})

const fileFromDatabase = (database: Database): File => {
  const exported = database.export()
  const bytes = new Uint8Array(exported.byteLength)
  bytes.set(exported)
  return new File([bytes.buffer], 'fixture.db', { type: 'application/vnd.sqlite3' })
}

describe('database compatibility boundary', () => {
  it('opens a schema-current teslalog database', async () => {
    const SQL = await initSqlJs()
    const database = new SQL.Database()
    database.exec(schema)
    database.run("INSERT INTO vehicles (vin, display_name) VALUES ('TESTVIN', 'Test car')")
    const loaded = await openDatabase(fileFromDatabase(database))
    database.close()
    expect(loaded.vehicle.displayName).toBe('Test car')
    expect(loaded.fileName).toBe('fixture.db')
  })

  it('rejects an older schema with the exact missing column', async () => {
    const SQL = await initSqlJs()
    const database = new SQL.Database()
    database.exec(schema)
    database.exec('ALTER TABLE drives DROP COLUMN start_location')
    await expect(openDatabase(fileFromDatabase(database))).rejects.toThrow('drives.start_location')
    database.close()
  })
})
