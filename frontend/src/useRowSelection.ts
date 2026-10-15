import { useEffect, useRef, useState } from 'react'

// Row selection for the Drives and Charges tables: checkboxes, shift-click
// ranges, select-all over what the filters show, Escape to clear. The
// selection is kept by id, so it survives re-sorting; rows hidden by a
// filter stay selected but are left out of the totals and reported apart.

export type SelectAllState = 'none' | 'some' | 'all'

// selectRange sets every id between the anchor and the clicked row, in the
// order the rows are shown, to the clicked row's new state - the shift-click
// behaviour of a mail client.
export const selectRange = (
  current: ReadonlySet<number>,
  order: readonly number[],
  anchor: number,
  target: number,
  select: boolean,
): ReadonlySet<number> => {
  const from = order.indexOf(anchor)
  const to = order.indexOf(target)
  const next = new Set(current)
  if (from < 0 || to < 0) {
    if (select) next.add(target)
    else next.delete(target)
    return next
  }
  for (const id of order.slice(Math.min(from, to), Math.max(from, to) + 1)) {
    if (select) next.add(id)
    else next.delete(id)
  }
  return next
}

export type RowSelection = Readonly<{
  isSelected: (id: number) => boolean
  visibleSelected: readonly number[]
  hiddenCount: number
  allState: SelectAllState
  toggle: (id: number, extend: boolean) => void
  toggleAll: () => void
  clear: () => void
}>

export const useRowSelection = (visibleIds: readonly number[]): RowSelection => {
  const [selected, setSelected] = useState<ReadonlySet<number>>(new Set())
  const anchor = useRef<number | null>(null)

  const visible = new Set(visibleIds)
  const visibleSelected = visibleIds.filter((id) => selected.has(id))
  const hiddenCount = [...selected].filter((id) => !visible.has(id)).length
  const allState: SelectAllState =
    visibleSelected.length === 0 ? 'none' : visibleSelected.length === visibleIds.length ? 'all' : 'some'

  const clear = (): void => {
    setSelected(new Set())
    anchor.current = null
  }

  useEffect(() => {
    if (selected.size === 0) return
    const onKey = (event: KeyboardEvent): void => {
      if (event.key === 'Escape') {
        setSelected(new Set())
        anchor.current = null
      }
    }
    globalThis.addEventListener('keydown', onKey)
    return () => globalThis.removeEventListener('keydown', onKey)
  }, [selected.size])

  return {
    isSelected: (id) => selected.has(id),
    visibleSelected,
    hiddenCount,
    allState,
    toggle: (id, extend) => {
      const select = !selected.has(id)
      // Read the anchor now: the updater below runs later, after the anchor
      // has already moved to this row.
      const from = extend ? anchor.current : null
      setSelected((current) =>
        from !== null
          ? selectRange(current, visibleIds, from, id, select)
          : selectRange(current, [], id, id, select),
      )
      anchor.current = id
    },
    // Select every row the filters show; if they are all selected already,
    // clear them (and only them).
    toggleAll: () => {
      setSelected((current) => {
        const next = new Set(current)
        if (allState === 'all') for (const id of visibleIds) next.delete(id)
        else for (const id of visibleIds) next.add(id)
        return next
      })
      anchor.current = null
    },
    clear,
  }
}
