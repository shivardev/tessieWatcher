import type { MouseEvent } from 'react'

// A drive or charge is addressable as ?drive=<id> / ?charge=<id>, so its
// details page has a real URL: ctrl/cmd/shift/middle-click on a link opens
// it in a new tab, the address can be shared or bookmarked, and the
// browser's back button returns to the list.
export type RecordRef = Readonly<{ kind: 'drive' | 'charge'; id: number }> | null

const kinds = ['drive', 'charge'] as const

export const recordFromSearch = (search: string): RecordRef => {
  const params = new URLSearchParams(search)
  for (const kind of kinds) {
    const raw = params.get(kind)
    if (raw !== null && /^\d+$/u.test(raw)) return { kind, id: Number(raw) }
  }
  return null
}

// recordHref returns the current address with only the record parameter
// changed, so unrelated parameters (such as ?cloud) survive.
export const recordHref = (
  record: RecordRef,
  current: string = globalThis.location?.href ?? 'http://localhost/',
): string => {
  const url = new URL(current)
  for (const kind of kinds) url.searchParams.delete(kind)
  if (record) url.searchParams.set(record.kind, String(record.id))
  return `${url.pathname}${url.search}${url.hash}`
}

// A click the browser should handle itself: open in a new tab or window,
// download, or anything other than a plain primary click.
export const opensElsewhere = (
  event: Pick<MouseEvent, 'button' | 'ctrlKey' | 'metaKey' | 'shiftKey' | 'altKey'>,
): boolean =>
  event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey
