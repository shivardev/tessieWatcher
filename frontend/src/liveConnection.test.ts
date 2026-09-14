import { describe, expect, it } from 'vitest'
import {
  hasNewData,
  normaliseBaseUrl,
  LiveConnectionError,
  type LiveMeta,
} from './liveConnection'

const meta = (overrides: Partial<LiveMeta> = {}): LiveMeta => ({
  lastUpdated: '2026-08-24T21:00:00Z',
  sizeBytes: 10_366_976,
  drives: 13,
  charges: 6,
  latestPositionId: 38_030,
  snapshotRevision: 'abc123',
  ...overrides,
})

describe('normaliseBaseUrl', () => {
  // People type the address off the sticker on the Pi, not a URL.
  it.each([
    ['10.0.0.236', 'http://10.0.0.236:8083'],
    ['10.0.0.236:8083', 'http://10.0.0.236:8083'],
    ['http://10.0.0.236:8083', 'http://10.0.0.236:8083'],
    ['http://10.0.0.236:8083/', 'http://10.0.0.236:8083'],
    ['  10.0.0.236:9000  ', 'http://10.0.0.236:9000'],
    ['teslalog.local', 'http://teslalog.local:8083'],
  ])('normalises %s', (input, expected) => {
    expect(normaliseBaseUrl(input)).toBe(expected)
  })

  // An explicit https address keeps the default port, since 8083 is
  // teslalog's plain-HTTP default and a reverse proxy will be on 443.
  it('leaves an https address on its own port', () => {
    expect(normaliseBaseUrl('https://tesla.example.com')).toBe('https://tesla.example.com')
  })

  it.each(['', '   '])('rejects blank input %p', (input) => {
    expect(() => normaliseBaseUrl(input)).toThrow(LiveConnectionError)
  })
})

describe('hasNewData', () => {
  it('always fetches the first time', () => {
    expect(hasNewData(null, meta())).toBe(true)
  })

  it('does not re-fetch when the revision is unchanged', () => {
    expect(hasNewData(meta(), meta())).toBe(false)
  })

  // The revision is the hash of the bytes /download would serve. It is the
  // only thing that should trigger a re-download.
  it('re-fetches when the snapshot revision changes', () => {
    expect(hasNewData(meta(), meta({ snapshotRevision: 'def456' }))).toBe(true)
  })

  // The whole point of the revision: while the car drives, the position id
  // moves and the file's size/mtime churn, but the served snapshot (and so
  // its revision) does not change until a trip closes. None of these may
  // trigger a re-download.
  it.each([
    ['an in-progress drive', { latestPositionId: 38_120 }],
    ['the file being rewritten', { sizeBytes: 10_370_000 }],
    ['the mtime moving', { lastUpdated: '2026-08-24T22:00:00Z' }],
    ['drives/charges counters (revision governs)', { drives: 14, charges: 7 }],
  ])('ignores %s while the revision holds', (_label, change) => {
    expect(hasNewData(meta(), meta(change))).toBe(false)
  })

  // An older portal that predates snapshot_revision reports "" for it; fall
  // back to the closed-row counters so such a portal still updates, while
  // the live position id still cannot drive a re-download.
  describe('legacy portal without a revision', () => {
    const legacy = (overrides: Partial<LiveMeta> = {}): LiveMeta =>
      meta({ snapshotRevision: '', ...overrides })
    it('re-fetches on a finished drive', () => {
      expect(hasNewData(legacy(), legacy({ drives: 14 }))).toBe(true)
    })
    it('re-fetches on a finished charge', () => {
      expect(hasNewData(legacy(), legacy({ charges: 7 }))).toBe(true)
    })
    it('ignores an in-progress drive', () => {
      expect(hasNewData(legacy(), legacy({ latestPositionId: 38_120 }))).toBe(false)
    })
  })
})
