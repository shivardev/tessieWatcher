import { useEffect, useRef, type MouseEvent } from 'react'
import { SquareCheckBig, X } from 'lucide-react'
import type { SelectAllState } from './useRowSelection'

// unit is printed small after the value; note is the hover explanation.
export type SelectionStat = Readonly<{
  label: string
  value: string
  unit?: string | undefined
  note?: string
}>

// A row's checkbox. Shift-click extends the selection from the last row
// clicked; the click never reaches the row link beside it.
export function RowCheckbox({
  checked,
  label,
  onToggle,
}: Readonly<{ checked: boolean; label: string; onToggle: (extend: boolean) => void }>) {
  return (
    <input
      type="checkbox"
      className="row-check"
      aria-label={label}
      checked={checked}
      onChange={() => undefined}
      onClick={(event: MouseEvent<HTMLInputElement>) => {
        event.stopPropagation()
        onToggle(event.shiftKey)
      }}
    />
  )
}

// The header checkbox: all, none, or a dash for some.
export function SelectAllCheckbox({
  state,
  label,
  onToggle,
}: Readonly<{ state: SelectAllState; label: string; onToggle: () => void }>) {
  const box = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (box.current) box.current.indeterminate = state === 'some'
  }, [state])
  return (
    <input
      ref={box}
      type="checkbox"
      className="row-check"
      aria-label={label}
      checked={state === 'all'}
      onChange={onToggle}
    />
  )
}

// Live totals for the selected rows: one slim line above the table that
// sticks under the top bar while the page scrolls, so the totals stay in
// view next to the rows being ticked without covering any of them. Longer
// explanations live in each figure's tooltip.
export function SelectionBanner({
  count,
  noun,
  hiddenCount,
  stats,
  onClear,
}: Readonly<{
  count: number
  noun: readonly [string, string]
  hiddenCount: number
  stats: readonly SelectionStat[]
  onClear: () => void
}>) {
  if (count === 0) return null
  return (
    <section className="selection-strip" aria-label="Totals for selected rows" aria-live="polite">
      <b className="selection-count" title="Shift-click selects a range. Esc clears.">
        <SquareCheckBig aria-hidden="true" />
        {count} {count === 1 ? noun[0] : noun[1]}
      </b>
      <dl>
        {stats.map((stat) => (
          <div key={stat.label} title={stat.note}>
            <dt>{stat.label}</dt>
            <dd>
              {stat.value}
              {stat.unit && <small> {stat.unit}</small>}
            </dd>
          </div>
        ))}
      </dl>
      {hiddenCount > 0 && (
        <small
          className="selection-hidden"
          title="Selected rows the current filters hide are not counted"
        >
          +{hiddenCount} hidden
        </small>
      )}
      <button type="button" onClick={onClear} title="Clear selection (Esc)">
        <X aria-hidden="true" />
        <span>Clear</span>
      </button>
    </section>
  )
}
