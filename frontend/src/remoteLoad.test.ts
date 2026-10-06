import { readFile } from 'node:fs/promises'
import initSqlJs, { type Database } from 'sql.js'
import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { loadSteps, openDatabaseRemote, type LoadStep } from './database'

const config = { baseUrl: 'http://server.test', databaseId: 'teslalog', apiKey: '' }
let database: Database

beforeAll(async () => {
  const source = await readFile('../internal/storage/schema.go', 'utf8')
  const schema = source.match(/const schema = `([\s\S]*?)`/)?.[1]
  if (!schema) throw new Error('Canonical schema not found')
  const SQL = await initSqlJs()
  database = new SQL.Database()
  database.exec(schema)
  database.run("INSERT INTO vehicles (vin, display_name) VALUES ('TESTVIN', 'Test car')")
})

afterEach(() => {
  vi.unstubAllGlobals()
})

// A data server stand-in: answers each POSTed query from an in-memory
// sql.js database after a short delay, and records peak concurrency.
const fakeServer = () => {
  let inFlight = 0
  let peak = 0
  const fetch = vi.fn(async (_url: string, init: RequestInit) => {
    inFlight += 1
    peak = Math.max(peak, inFlight)
    try {
      await new Promise((resolve) => setTimeout(resolve, 5))
      const { query } = JSON.parse(String(init.body)) as { query: string }
      const result = database.exec(query)[0]
      return new Response(
        JSON.stringify({ columns: result?.columns ?? [], rows: result?.values ?? [] }),
        { headers: { 'Content-Type': 'application/json' } },
      )
    } finally {
      inFlight -= 1
    }
  })
  return { fetch, peak: () => peak }
}

describe('remote database loading', () => {
  it('reports every step, vehicle first, and caps concurrent requests', async () => {
    const server = fakeServer()
    vi.stubGlobal('fetch', server.fetch)
    const steps: LoadStep[] = []
    const loaded = await openDatabaseRemote(config, { onStep: (step) => steps.push(step) })
    expect(loaded.vehicle.displayName).toBe('Test car')
    expect(steps[0]).toBe('vehicle')
    expect([...steps].sort()).toEqual(loadSteps.map((step) => step.key).sort())
    expect(server.peak()).toBeGreaterThan(1)
    expect(server.peak()).toBeLessThanOrEqual(4)
  })

  it('stops promptly when cancelled instead of waiting on the server', async () => {
    // A server that never answers until the request is aborted.
    vi.stubGlobal(
      'fetch',
      vi.fn(
        (_url: string, init: RequestInit) =>
          new Promise((_, reject) => {
            init.signal?.addEventListener('abort', () => reject(init.signal?.reason))
          }),
      ),
    )
    const controller = new AbortController()
    const pending = openDatabaseRemote(config, { signal: controller.signal })
    setTimeout(() => controller.abort(), 20)
    await expect(pending).rejects.toThrow('Cancelled.')
  })
})
