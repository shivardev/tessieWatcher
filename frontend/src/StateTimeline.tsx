import { useMemo, useState } from 'react'
import type { QueryValue } from './domain'

// A vehicle-state timeline: what the car was doing, when, and for how
// long.
//
// This replaced three separate hand-rolled versions that rendered a bare
// strip of coloured blocks with the state name in a `title` attribute
// and nothing else - no legend, no time axis, no durations. The colours
// were CSS classes keyed off a mangled state string, so a state whose
// class was missing rendered as an invisible gap rather than an obvious
// unknown. You could see that something changed and never what, or when.
//
// Everything here exists to answer a question the strip could not:
//   which colour is which state      -> legend, with time and share
//   when did that happen             -> axis with real tick labels
//   how long was the car asleep      -> per-segment duration on hover
//   is that sliver real or a gap     -> minimum width, explicit colour

export type StateSpan = Readonly<{
  /** Epoch milliseconds. */
  start: number
  /** Epoch milliseconds. Must be >= start. */
  end: number
  state: string
}>

// Colours are data, not CSS classes, so an unrecognised state gets a
// deliberate fallback instead of disappearing. Grouped by meaning:
// blue/green for the car doing something, warm grey for correctly
// sleeping, purple for unreachable.
const stateColors: ReadonlyMap<string, string> = new Map([
  ['driving', '#5794f2'],
  ['charging', '#73bf69'],
  ['charging (ac)', '#73bf69'],
  ['charging (dc)', '#fade2a'],
  ['online', '#7ebdc9'],
  ['idle', '#4e8a97'],
  ['asleep', '#f0b35a'],
  ['suspended', '#c79a4a'],
  ['offline', '#8b6daa'],
  ['updating', '#f2495c'],
])
const unknownColor = '#5a6b68'

export const stateColor = (state: string): string =>
  stateColors.get(state.trim().toLowerCase()) ?? unknownColor

export const humanDuration = (milliseconds: number): string => {
  const minutes = milliseconds / 60_000
  if (minutes < 1) return `${Math.round(milliseconds / 1000)}s`
  if (minutes < 60) return `${Math.round(minutes)}m`
  const hours = minutes / 60
  if (hours < 24) {
    const wholeHours = Math.floor(hours)
    const remainder = Math.round(minutes - wholeHours * 60)
    return remainder === 0 ? `${wholeHours}h` : `${wholeHours}h ${remainder}m`
  }
  const days = Math.floor(hours / 24)
  const remainderHours = Math.round(hours - days * 24)
  return remainderHours === 0 ? `${days}d` : `${days}d ${remainderHours}h`
}

// Ticks are chosen from a fixed ladder so labels land on times a person
// recognises - the top of an hour, midnight - rather than at arbitrary
// offsets from whenever the window happens to begin.
const minute = 60_000
const hour = 60 * minute
const day = 24 * hour
const tickSteps = [
  5 * minute,
  15 * minute,
  30 * minute,
  hour,
  2 * hour,
  3 * hour,
  6 * hour,
  12 * hour,
  day,
  2 * day,
  3 * day,
  7 * day,
  14 * day,
  28 * day,
] as const

export const axisTicks = (start: number, end: number, target = 6): readonly number[] => {
  const span = end - start
  if (span <= 0) return []
  // The step whose tick count lands CLOSEST to the target, not the first
  // one below it. Taking the first under target walked off the end of the
  // ladder: a ten-day window skipped the 24-hour step (ten ticks) for the
  // seven-day one and rendered a single label.
  const step =
    tickSteps.toSorted(
      (left, right) => Math.abs(span / left - target) - Math.abs(span / right - target),
    )[0] ?? span / target
  const ticks: number[] = []
  // Align to the step so ticks fall on round local times. Date is used
  // rather than modular arithmetic on the epoch because a day-sized step
  // has to align to local midnight, which the epoch does not know about.
  const first = new Date(start)
  if (step >= 24 * 60 * 60_000) first.setHours(0, 0, 0, 0)
  else first.setMinutes(0, 0, 0)
  for (let at = first.getTime(); at <= end; at += step) {
    if (at >= start) ticks.push(at)
  }
  return ticks
}

const tickLabel = (at: number, span: number): string => {
  const date = new Date(at)
  if (span > 3 * 24 * 60 * 60_000) {
    return date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  }
  if (span > 24 * 60 * 60_000) {
    return date.toLocaleString(undefined, { weekday: 'short', hour: 'numeric' })
  }
  return date.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })
}

export function StateTimeline({
  spans,
  title,
  emptyMessage = 'No state history in this period.',
}: Readonly<{ spans: readonly StateSpan[]; title?: string; emptyMessage?: string }>) {
  const [hovered, setHovered] = useState<number | null>(null)

  const model = useMemo(() => {
    const valid = spans
      .filter((span) => Number.isFinite(span.start) && Number.isFinite(span.end) && span.end >= span.start)
      .toSorted((left, right) => left.start - right.start)
    if (valid.length === 0) return null

    const start = valid[0]!.start
    const end = Math.max(...valid.map((span) => span.end), start + 1)
    const span = end - start

    // Totals drive the legend, so it reports where the time actually
    // went rather than merely which colours appear.
    const totals = new Map<string, number>()
    for (const item of valid) {
      totals.set(item.state, (totals.get(item.state) ?? 0) + (item.end - item.start))
    }
    const covered = [...totals.values()].reduce((sum, value) => sum + value, 0)

    return {
      spans: valid,
      start,
      end,
      span,
      ticks: axisTicks(start, end),
      legend: [...totals.entries()]
        .toSorted((left, right) => right[1] - left[1])
        .map(([state, duration]) => ({
          state,
          duration,
          share: covered === 0 ? 0 : (duration / covered) * 100,
        })),
    }
  }, [spans])

  if (model === null) {
    return (
      <section className="state-timeline-panel">
        {title !== undefined && <h2>{title}</h2>}
        <p className="no-data">{emptyMessage}</p>
      </section>
    )
  }

  const active = hovered === null ? undefined : model.spans[hovered]

  return (
    <section className="state-timeline-panel">
      {title !== undefined && <h2>{title}</h2>}

      <div
        className="state-timeline-track"
        role="img"
        aria-label={`Vehicle state from ${new Date(model.start).toLocaleString()} to ${new Date(model.end).toLocaleString()}`}
        onMouseLeave={() => setHovered(null)}
      >
        {model.spans.map((item, index) => {
          const left = ((item.start - model.start) / model.span) * 100
          // A floor on width so a genuinely short state stays visible
          // and clickable instead of collapsing into the background.
          const width = Math.max(0.35, ((item.end - item.start) / model.span) * 100)
          return (
            <i
              key={`${item.start}-${index}`}
              style={{ left: `${left}%`, width: `${width}%`, background: stateColor(item.state) }}
              className={hovered === index ? 'is-hovered' : undefined}
              onMouseEnter={() => setHovered(index)}
            />
          )
        })}
      </div>

      <div className="state-timeline-axis" aria-hidden="true">
        {model.ticks.map((at) => (
          <span key={at} style={{ left: `${((at - model.start) / model.span) * 100}%` }}>
            {tickLabel(at, model.span)}
          </span>
        ))}
      </div>

      {/* Reserved space rather than a floating tooltip: the readout never
          covers the track it describes, and the layout does not jump as
          the cursor moves across segments. */}
      <p className="state-timeline-readout">
        {active === undefined ? (
          <span className="hint">Hover the timeline for the exact state, time and duration.</span>
        ) : (
          <>
            <i style={{ background: stateColor(active.state) }} />
            <b>{active.state}</b>
            <span>{new Date(active.start).toLocaleString()}</span>
            <span>→ {new Date(active.end).toLocaleTimeString()}</span>
            <em>{humanDuration(active.end - active.start)}</em>
          </>
        )}
      </p>

      <ul className="state-timeline-legend">
        {model.legend.map((entry) => (
          <li key={entry.state}>
            <i style={{ background: stateColor(entry.state) }} />
            <span>{entry.state}</span>
            <b>{humanDuration(entry.duration)}</b>
            <em>{entry.share.toFixed(entry.share < 10 ? 1 : 0)}%</em>
          </li>
        ))}
      </ul>
    </section>
  )
}

// spansFromRows builds spans from a query that returns one row per state
// CHANGE rather than per interval: each row's end is the next row's
// start. Used by the catalog dashboards, whose state-timeline panels are
// written as plain "time, state" series so the same SQL also works in
// Grafana.
export const spansFromRows = (
  rows: readonly (readonly QueryValue[])[],
  now = Date.now(),
): readonly StateSpan[] =>
  rows.flatMap((row, index) => {
    const at = row[0]
    const state = row[1]
    if (typeof state !== 'string') return []
    const start = typeof at === 'number' ? at : typeof at === 'string' ? Date.parse(at) : Number.NaN
    if (!Number.isFinite(start)) return []
    const nextRaw = rows[index + 1]?.[0]
    const next =
      typeof nextRaw === 'number'
        ? nextRaw
        : typeof nextRaw === 'string'
          ? Date.parse(nextRaw)
          : Number.NaN
    return [{ start, end: Number.isFinite(next) ? next : now, state }]
  })
