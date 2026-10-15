import { describe, expect, it } from 'vitest'
import { opensElsewhere, recordFromSearch, recordHref } from './recordLinks'

describe('record links', () => {
  it('builds addresses that keep unrelated parameters', () => {
    const current = 'https://example.test/app/?cloud=1&charge=4#top'
    expect(recordHref({ kind: 'drive', id: 12 }, current)).toBe('/app/?cloud=1&drive=12#top')
    expect(recordHref(null, current)).toBe('/app/?cloud=1#top')
  })

  it('reads only well-formed record ids back from the address', () => {
    expect(recordFromSearch('?drive=12')).toEqual({ kind: 'drive', id: 12 })
    expect(recordFromSearch('?charge=7&cloud')).toEqual({ kind: 'charge', id: 7 })
    expect(recordFromSearch('?drive=12;DROP')).toBeNull()
    expect(recordFromSearch('')).toBeNull()
  })

  it('leaves modified and non-primary clicks to the browser', () => {
    const plain = { button: 0, ctrlKey: false, metaKey: false, shiftKey: false, altKey: false }
    expect(opensElsewhere(plain)).toBe(false)
    expect(opensElsewhere({ ...plain, ctrlKey: true })).toBe(true)
    expect(opensElsewhere({ ...plain, metaKey: true })).toBe(true)
    expect(opensElsewhere({ ...plain, button: 1 })).toBe(true)
  })
})
