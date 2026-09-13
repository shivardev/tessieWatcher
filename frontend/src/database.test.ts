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

  it('uses TeslaMate Charges semantics for each row when used energy is lower or null', async () => {
    const SQL = await initSqlJs()
    const database = new SQL.Database()
    database.exec(schema)
    database.run("INSERT INTO vehicles (vin, display_name) VALUES ('TESTVIN', 'Test car')")
    database.run(`INSERT INTO charging_sessions
      (vehicle_id,start_time,end_time,charge_energy_added_kwh,charge_energy_used_kwh,cost,status)
      VALUES (1,'2026-09-12T10:00:00Z','2026-09-12T11:00:00Z',5,4,1,'closed'),
             (1,'2026-09-12T12:00:00Z','2026-09-12T13:00:00Z',3,NULL,0.6,'closed'),
             (1,'2026-09-12T14:00:00Z','2026-09-12T15:00:00Z',2,2.5,0.5,'closed')`)
    const loaded = await openDatabase(fileFromDatabase(database))
    database.close()
    expect(loaded.charges.map((charge) => charge.energyAddedKwh)).toEqual([2, 3, 5])
    expect(loaded.charges.map((charge) => charge.energyUsedKwh)).toEqual([2.5, 3, 5])
    expect(loaded.charges.map((charge) => charge.efficiencyPercent)).toEqual([80, 100, 100])
    for (const charge of loaded.charges) expect(charge.costPerKwh).toBeCloseTo(0.2)
  })
})
