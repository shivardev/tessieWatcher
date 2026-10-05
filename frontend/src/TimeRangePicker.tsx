import { useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react'
import {
  CalendarDays,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  ChevronsLeft,
  ChevronsRight,
  CircleAlert,
  Clock,
  Search,
  X,
  ZoomOut,
} from 'lucide-react'
import {
  formatTimeInput,
  parseTimeExpression,
  quickRanges,
  rangeFromInputs,
  shiftTimeRange,
  timeRangeInputs,
  timeRangeLabel,
  zoomOutTimeRange,
  type ViewSettingsTime,
} from './viewSettings'

// A Grafana-style time picker: quick relative ranges, an absolute From/To
// that also accepts "now"/"now-7d", a calendar, and shift/zoom buttons.

const recentKey = 'teslalog.viewer.recentRanges'
const weekdays = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun']
const invalidMessage = 'Enter a date like 2026-10-02 00:00:00, or "now" / "now-7d"'

type Recent = Readonly<{ from: string; to: string }>

// Recently applied absolute ranges are a per-browser convenience; storage
// can be missing or blocked, in which case the list is simply empty.
const loadRecent = (): readonly Recent[] => {
  try {
    const parsed: unknown = JSON.parse(globalThis.localStorage?.getItem(recentKey) ?? '[]')
    return Array.isArray(parsed)
      ? parsed.filter(
          (item): item is Recent => typeof item?.from === 'string' && typeof item?.to === 'string',
        )
      : []
  } catch {
    return []
  }
}
const saveRecent = (recent: readonly Recent[]): void => {
  try {
    globalThis.localStorage?.setItem(recentKey, JSON.stringify(recent))
  } catch {
    // Storage unavailable; recent ranges just will not persist.
  }
}

const startOfDay = (date: Date): Date =>
  new Date(date.getFullYear(), date.getMonth(), date.getDate())
const sameDay = (a: Date | null, b: Date): boolean =>
  a !== null && startOfDay(a).getTime() === b.getTime()

// Six Monday-first weeks covering the month, as Grafana's calendar shows.
const monthGrid = (month: Date): readonly Date[] => {
  const first = new Date(month.getFullYear(), month.getMonth(), 1)
  const offset = (first.getDay() + 6) % 7
  return Array.from(
    { length: 42 },
    (_, index) => new Date(first.getFullYear(), first.getMonth(), index - offset + 1),
  )
}

const utcOffset = (): string => {
  const minutes = -new Date().getTimezoneOffset()
  const sign = minutes < 0 ? '-' : '+'
  const absolute = Math.abs(minutes)
  return `UTC${sign}${String(Math.floor(absolute / 60)).padStart(2, '0')}:${String(absolute % 60).padStart(2, '0')}`
}

function IconButton({
  label,
  disabled,
  onClick,
  children,
}: Readonly<{ label: string; disabled?: boolean; onClick: () => void; children: ReactNode }>) {
  return (
    <button
      type="button"
      className="time-picker-icon"
      aria-label={label}
      title={label}
      disabled={disabled}
      onClick={onClick}
    >
      {children}
    </button>
  )
}

export function TimeRangePicker({
  value,
  onChange,
}: Readonly<{ value: ViewSettingsTime; onChange: (next: ViewSettingsTime) => void }>) {
  const [open, setOpen] = useState(false)
  const [from, setFrom] = useState('')
  const [to, setTo] = useState('')
  const [errors, setErrors] = useState<Readonly<{ from?: string; to?: string }>>({})
  const [calendarFor, setCalendarFor] = useState<'from' | 'to' | null>(null)
  const [month, setMonth] = useState(() => startOfDay(new Date()))
  const [search, setSearch] = useState('')
  const [recent, setRecent] = useState<readonly Recent[]>(loadRecent)
  const root = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    const onPointer = (event: PointerEvent): void => {
      if (!root.current?.contains(event.target as Node)) setOpen(false)
    }
    const onKey = (event: KeyboardEvent): void => {
      if (event.key === 'Escape') setOpen(false)
    }
    document.addEventListener('pointerdown', onPointer)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('pointerdown', onPointer)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  const toggle = (): void => {
    if (open) {
      setOpen(false)
      return
    }
    const inputs = timeRangeInputs(value)
    setFrom(inputs.from)
    setTo(inputs.to)
    setErrors({})
    setCalendarFor(null)
    setSearch('')
    setMonth(startOfDay(parseTimeExpression(inputs.from, 'from') ?? new Date()))
    setOpen(true)
  }

  const choose = (next: ViewSettingsTime): void => {
    onChange(next)
    setOpen(false)
  }

  const apply = (event?: FormEvent): void => {
    event?.preventDefault()
    const fromDate = parseTimeExpression(from, 'from')
    const toDate = parseTimeExpression(to, 'to')
    const nextErrors = {
      ...(fromDate === null ? { from: invalidMessage } : {}),
      ...(toDate === null ? { to: invalidMessage } : {}),
    }
    if (fromDate !== null && toDate !== null && fromDate.getTime() > toDate.getTime())
      Object.assign(nextErrors, { from: '"From" must be before "To"' })
    setErrors(nextErrors)
    if (Object.keys(nextErrors).length > 0) return
    const next = rangeFromInputs(from, to)
    if (next.timeRange === 'custom') {
      const entry = { from: next.customFrom, to: next.customTo }
      const updated = [
        entry,
        ...recent.filter((item) => item.from !== entry.from || item.to !== entry.to),
      ].slice(0, 4)
      setRecent(updated)
      saveRecent(updated)
    }
    choose(next)
  }

  const pickDay = (day: Date): void => {
    const start = formatTimeInput(day)
    const end = formatTimeInput(
      new Date(day.getFullYear(), day.getMonth(), day.getDate(), 23, 59, 59),
    )
    if (calendarFor === 'from') {
      setFrom(start)
      const currentTo = parseTimeExpression(to, 'to')
      if (currentTo !== null && currentTo.getTime() < day.getTime()) setTo(end)
      // Like Grafana: the first click sets From, the second sets To.
      setCalendarFor('to')
    } else {
      const currentFrom = parseTimeExpression(from, 'from')
      if (currentFrom !== null && currentFrom.getTime() > day.getTime()) {
        setFrom(start)
        setTo(
          formatTimeInput(
            new Date(
              currentFrom.getFullYear(),
              currentFrom.getMonth(),
              currentFrom.getDate(),
              23,
              59,
              59,
            ),
          ),
        )
      } else setTo(end)
      setCalendarFor(null)
    }
    setErrors({})
  }

  const query = search.trim().toLowerCase()
  const visibleRanges = [
    ...quickRanges.map((range) => ({ key: range.key, label: range.label })),
    { key: 'all' as const, label: 'All time' },
  ].filter((range) => query === '' || range.label.toLowerCase().includes(query))

  const fromDate = parseTimeExpression(from, 'from')
  const toDate = parseTimeExpression(to, 'to')
  const today = startOfDay(new Date())
  const shiftable = value.timeRange !== 'all'
  const zone = Intl.DateTimeFormat().resolvedOptions().timeZone

  return (
    <div className="time-picker" ref={root}>
      <IconButton
        label="Move time range backward"
        disabled={!shiftable}
        onClick={() => {
          const next = shiftTimeRange(value, -1)
          if (next) onChange(next)
        }}
      >
        <ChevronsLeft />
      </IconButton>
      <button
        type="button"
        className={`time-picker-trigger${open ? ' open' : ''}`}
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={toggle}
      >
        <Clock />
        <span>{timeRangeLabel(value)}</span>
        <ChevronDown className="chevron" />
      </button>
      <IconButton
        label="Move time range forward"
        disabled={!shiftable || value.timeRange !== 'custom' || value.customTo.trim() === 'now'}
        onClick={() => {
          const next = shiftTimeRange(value, 1)
          if (next) onChange(next)
        }}
      >
        <ChevronsRight />
      </IconButton>
      <IconButton
        label="Zoom out time range"
        disabled={!shiftable}
        onClick={() => {
          const next = zoomOutTimeRange(value)
          if (next) onChange(next)
        }}
      >
        <ZoomOut />
      </IconButton>

      {open && (
        <div className="time-picker-popover" role="dialog" aria-label="Time range">
          <div className="time-picker-panels">
            {calendarFor && (
              <div className="time-picker-calendar">
                <header>
                  <b>Select a time range</b>
                  <IconButton label="Close calendar" onClick={() => setCalendarFor(null)}>
                    <X />
                  </IconButton>
                </header>
                <nav>
                  <IconButton
                    label="Previous month"
                    onClick={() => setMonth(new Date(month.getFullYear(), month.getMonth() - 1, 1))}
                  >
                    <ChevronLeft />
                  </IconButton>
                  <span>
                    {new Intl.DateTimeFormat(undefined, { month: 'long', year: 'numeric' }).format(
                      month,
                    )}
                  </span>
                  <IconButton
                    label="Next month"
                    onClick={() => setMonth(new Date(month.getFullYear(), month.getMonth() + 1, 1))}
                  >
                    <ChevronRight />
                  </IconButton>
                </nav>
                <div className="calendar-grid" role="grid">
                  {weekdays.map((day) => (
                    <span key={day} className="weekday">
                      {day}
                    </span>
                  ))}
                  {monthGrid(month).map((day) => {
                    const inRange =
                      fromDate !== null &&
                      toDate !== null &&
                      day.getTime() >= startOfDay(fromDate).getTime() &&
                      day.getTime() <= startOfDay(toDate).getTime()
                    const edge = sameDay(fromDate, day) || sameDay(toDate, day)
                    const classes = [
                      day.getMonth() === month.getMonth() ? '' : 'outside',
                      inRange ? 'in-range' : '',
                      edge ? 'edge' : '',
                      day.getTime() === today.getTime() ? 'today' : '',
                    ]
                    return (
                      <button
                        key={day.getTime()}
                        type="button"
                        className={classes.filter(Boolean).join(' ')}
                        aria-pressed={edge}
                        onClick={() => pickDay(day)}
                      >
                        {day.getDate()}
                      </button>
                    )
                  })}
                </div>
                <p className="calendar-hint">
                  Picking {calendarFor === 'from' ? 'the start' : 'the end'} of the range
                </p>
              </div>
            )}

            <div className="time-picker-body">
              <form className="time-picker-absolute" onSubmit={apply} noValidate>
                <h3>Absolute time range</h3>
                {(['from', 'to'] as const).map((side) => (
                  <div key={side} className="time-picker-field">
                    <label htmlFor={`time-${side}`}>{side === 'from' ? 'From' : 'To'}</label>
                    <div className={`time-input${errors[side] ? ' invalid' : ''}`}>
                      <input
                        id={`time-${side}`}
                        value={side === 'from' ? from : to}
                        placeholder={side === 'from' ? 'now-7d' : 'now'}
                        spellCheck={false}
                        autoComplete="off"
                        aria-invalid={errors[side] !== undefined}
                        onChange={(event) => {
                          if (side === 'from') setFrom(event.target.value)
                          else setTo(event.target.value)
                          setErrors((current) => ({ ...current, [side]: undefined }))
                        }}
                      />
                      <IconButton
                        label={`Open calendar for ${side === 'from' ? 'From' : 'To'}`}
                        onClick={() => {
                          const anchor = parseTimeExpression(side === 'from' ? from : to, side)
                          if (anchor) setMonth(startOfDay(anchor))
                          setCalendarFor((current) => (current === side ? null : side))
                        }}
                      >
                        <CalendarDays />
                      </IconButton>
                    </div>
                    {errors[side] && (
                      <p className="time-picker-error" role="alert">
                        <CircleAlert /> {errors[side]}
                      </p>
                    )}
                  </div>
                ))}
                <button type="submit" className="time-picker-apply">
                  Apply time range
                </button>
                {recent.length > 0 ? (
                  <div className="time-picker-recent">
                    <h4>Recently used absolute ranges</h4>
                    {recent.map((item) => (
                      <button
                        key={`${item.from}|${item.to}`}
                        type="button"
                        onClick={() => choose(rangeFromInputs(item.from, item.to))}
                      >
                        {timeRangeLabel({
                          timeRange: 'custom',
                          customFrom: item.from,
                          customTo: item.to,
                        })}
                      </button>
                    ))}
                  </div>
                ) : (
                  <p className="time-picker-hint">
                    Type a date and time as <code>2026-10-02 08:30</code>, or a relative time such
                    as <code>now-6h</code>, <code>now-2d</code>, <code>now-3M</code>. Ranges you
                    apply will appear here.
                  </p>
                )}
              </form>

              <div className="time-picker-quick">
                <label className="time-picker-search">
                  <Search />
                  <input
                    value={search}
                    placeholder="Search quick ranges"
                    aria-label="Search quick ranges"
                    onChange={(event) => setSearch(event.target.value)}
                  />
                </label>
                <ul>
                  {visibleRanges.map((range) => (
                    <li key={range.key}>
                      <button
                        type="button"
                        className={range.key === value.timeRange ? 'active' : ''}
                        onClick={() =>
                          choose({ timeRange: range.key, customFrom: '', customTo: '' })
                        }
                      >
                        {range.label}
                      </button>
                    </li>
                  ))}
                  {visibleRanges.length === 0 && <li className="empty">No matching range</li>}
                </ul>
              </div>
            </div>
          </div>

          <footer>
            <b>Browser time</b>
            <span>{zone}</span>
            <em>{utcOffset()}</em>
          </footer>
        </div>
      )}
    </div>
  )
}
