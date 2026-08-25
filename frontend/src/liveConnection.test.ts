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

  it('does not re-fetch when nothing has changed', () => {
    expect(hasNewData(meta(), meta())).toBe(false)
  })

  // latestPositionId moves continuously while the car is driving, which
  // is what makes an in-progress drive visible without waiting for it to
  // close. Drives and charges count closed rows only, so they tick
  // exactly when new history becomes available.
  it.each([
    ['a finished drive', { drives: 14 }],
    ['a finished charge', { charges: 7 }],
    ['an in-progress drive', { latestPositionId: 38_120 }],
  ])('re-fetches after %s', (_label, change) => {
    expect(hasNewData(meta(), meta(change))).toBe(true)
  })

  // The snapshot is rebuilt on every /download, so its byte size and
  // mtime change even when the data has not. Keying on them would
  // re-download 10 MB a minute forever.
  it.each([
    ['the file being rewritten', { sizeBytes: 10_370_000 }],
    ['the mtime moving', { lastUpdated: '2026-08-24T22:00:00Z' }],
  ])('ignores %s', (_label, change) => {
    expect(hasNewData(meta(), meta(change))).toBe(false)
  })
})
