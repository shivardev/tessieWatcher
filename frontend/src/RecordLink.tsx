import type { ReactNode } from 'react'
import { opensElsewhere, recordHref, type RecordRef } from './recordLinks'

// A real <a href> to a drive or charge: a plain click opens it in place, and
// ctrl/cmd/shift/middle-click or "Open in new tab" are left to the browser.
export function RecordLink({
  record,
  onOpen,
  children,
}: Readonly<{ record: NonNullable<RecordRef>; onOpen: () => void; children: ReactNode }>) {
  return (
    <a
      className="table-link"
      href={recordHref(record)}
      onClick={(event) => {
        if (opensElsewhere(event)) return
        event.preventDefault()
        onOpen()
      }}
    >
      {children}
    </a>
  )
}
