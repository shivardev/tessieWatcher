import { afterEach, describe, expect, it, vi } from 'vitest'
import { normaliseResult, runRemoteQuery, runRemoteStatement, type RemoteConfig } from './remoteBackend'

const config: RemoteConfig = {
  baseUrl: 'https://teslalog.example.com',
  databaseId: '8bf46fc7-a47c-4b6d-89c6-8814989a6990',
  apiKey: 'sk_test_123',
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('normaliseResult', () => {
  it('accepts a columns + rows-of-arrays shape', () => {
    expect(
      normaliseResult({ columns: ['band', 'seconds'], rows: [[10, 42.5], [20, 8]] }),
    ).toEqual({ columns: ['band', 'seconds'], rows: [[10, 42.5], [20, 8]] })
  })

  // Some compatible data servers return a columns array plus rows of objects,
  // with every value a string. Values must map by column and numerics must
  // come back as numbers so the dashboards compute.
  it('handles columns plus rows-of-objects with stringified numbers', () => {
    expect(
      normaliseResult({ columns: ['n', 'loc'], rows: [{ n: '1', loc: 'Home' }], rowCount: 1 }),
    ).toEqual({ columns: ['n', 'loc'], rows: [[1, 'Home']] })
  })

  it('coerces numeric strings but leaves dates, text and zero-padded values alone', () => {
    const r = normaliseResult([{ a: '1.5', b: '-3', c: '2026-08-21T17:02:26.554Z', d: 'Home', e: '01234', f: '0' }])
    expect(r.rows[0]).toEqual([1.5, -3, '2026-08-21T17:02:26.554Z', 'Home', '01234', 0])
  })

  it('accepts an array of row objects and preserves column order', () => {
    expect(normaliseResult([{ Location: 'Home', Visits: 3 }, { Location: 'Work', Visits: 1 }])).toEqual({
      columns: ['Location', 'Visits'],
      rows: [
        ['Home', 3],
        ['Work', 1],
      ],
    })
  })

  it('unwraps row objects nested under rows/results/data', () => {
    for (const key of ['rows', 'results', 'data']) {
      expect(normaliseResult({ [key]: [{ a: 1 }] })).toEqual({ columns: ['a'], rows: [[1]] })
    }
  })

  it('coerces booleans to 1/0 and missing values to null', () => {
    expect(normaliseResult([{ flag: true, other: false, missing: null }])).toEqual({
      columns: ['flag', 'other', 'missing'],
      rows: [[1, 0, null]],
    })
  })

  it('returns an empty result for an empty set', () => {
    expect(normaliseResult([])).toEqual({ columns: [], rows: [] })
    expect(normaliseResult({ rows: [] })).toEqual({ columns: [], rows: [] })
  })

  it('flags a shape it cannot read', () => {
    expect(normaliseResult(42).error).toBeDefined()
  })
})

describe('runRemoteQuery', () => {
  it('posts the SQL with the bearer token to the database query endpoint', async () => {
    const fetchMock = vi.fn(async () => new Response(JSON.stringify([{ n: 1 }]), { status: 200 }))
    vi.stubGlobal('fetch', fetchMock)

    const result = await runRemoteQuery(config, 'SELECT 1 AS n')

    expect(fetchMock).toHaveBeenCalledOnce()
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toBe(
      'https://teslalog.example.com/v1/databases/8bf46fc7-a47c-4b6d-89c6-8814989a6990/query',
    )
    expect((init.headers as Record<string, string>).Authorization).toBe('Bearer sk_test_123')
    expect(JSON.parse(init.body as string)).toEqual({ query: 'SELECT 1 AS n' })
    expect(result).toEqual({ columns: ['n'], rows: [[1]] })
  })

  it('omits authorization when the optional token is empty', async () => {
    const fetchMock = vi.fn(async () => new Response(JSON.stringify([{ n: 1 }]), { status: 200 }))
    vi.stubGlobal('fetch', fetchMock)

    await runRemoteQuery({ ...config, apiKey: '' }, 'SELECT 1 AS n')

    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    expect((init.headers as Record<string, string>).Authorization).toBeUndefined()
  })

  it('captures an API error body instead of throwing', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(JSON.stringify({ error: 'no such table: drives' }), { status: 400 })),
    )
    const result = await runRemoteQuery(config, 'SELECT * FROM drives')
    expect(result.rows).toEqual([])
    expect(result.error).toBe('no such table: drives')
  })

  it('captures a network failure', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new TypeError('failed to fetch')
      }),
    )
    const result = await runRemoteQuery(config, 'SELECT 1')
    expect(result.error).toMatch(/could not reach/iu)
  })
})

describe('runRemoteStatement', () => {
  it('accepts an empty successful response from a write', async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }))
    vi.stubGlobal('fetch', fetchMock)
    await expect(runRemoteStatement(config, "DELETE FROM geofences WHERE id=1")).resolves.toBeUndefined()
    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    expect(JSON.parse(init.body as string)).toEqual({ query: 'DELETE FROM geofences WHERE id=1' })
  })
})
