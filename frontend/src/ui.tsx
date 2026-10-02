import { useEffect, useId, useRef, useState, type CSSProperties, type ReactNode } from 'react'
import type { LucideIcon } from 'lucide-react'
import { Cell, Pie, PieChart, ResponsiveContainer, Tooltip } from 'recharts'

// Shared visual primitives for the AMOLED theme: animated stat tiles, the
// battery ring, sparklines and the chart palette every dashboard draws
// with. Colours live here (and as CSS tokens in App.css) so charts and
// tiles read as one system instead of each panel picking its own hex.

export const palette = {
  lime: '#c6ff3d',
  cyan: '#22d3ee',
  violet: '#a78bfa',
  amber: '#fbbf24',
  rose: '#fb7185',
  emerald: '#34d399',
  blue: '#60a5fa',
  orange: '#fb923c',
} as const
export type Tone = keyof typeof palette

// Series order for multi-line charts: high-contrast neighbours first so the
// two most common series (usually value + one comparison) never look alike.
export const seriesPalette: readonly string[] = [
  palette.cyan,
  palette.lime,
  palette.violet,
  palette.amber,
  palette.rose,
  palette.emerald,
  palette.blue,
  palette.orange,
]

export const chartTheme = {
  grid: 'rgba(255,255,255,0.06)',
  axis: '#6b6b76',
  tick: { fontSize: 11, fill: '#7c7c88' },
  cursor: { stroke: 'rgba(255,255,255,0.22)', strokeWidth: 1 },
  tooltip: {
    background: 'rgba(12,12,14,0.92)',
    border: '1px solid rgba(255,255,255,0.1)',
    borderRadius: 12,
    boxShadow: '0 12px 40px rgba(0,0,0,0.6)',
    backdropFilter: 'blur(12px)',
    color: '#ececf1',
    fontSize: 12,
    padding: '8px 12px',
  } satisfies CSSProperties,
  tooltipLabel: { color: '#9b9ba7', marginBottom: 4 } satisfies CSSProperties,
} as const

// Mount animations are a nice touch on a few hundred points and a stutter on
// thousands, so charts only animate below this size.
export const animateBelow = 600

const prefersReducedMotion = (): boolean =>
  globalThis.matchMedia?.('(prefers-reduced-motion: reduce)').matches ?? false

// useCountUp eases a number from 0 to its target once, when it first
// appears or changes. Formatting stays with the caller; this only supplies
// the in-between values.
export function useCountUp(target: number, duration = 900): number {
  const [value, setValue] = useState(() => (prefersReducedMotion() ? target : 0))
  const from = useRef(0)
  useEffect(() => {
    if (!Number.isFinite(target) || prefersReducedMotion()) {
      setValue(target)
      return
    }
    const start = performance.now()
    const origin = from.current
    let frame = 0
    const step = (now: number): void => {
      const t = Math.min(1, (now - start) / duration)
      const eased = 1 - Math.pow(1 - t, 4)
      setValue(origin + (target - origin) * eased)
      if (t < 1) frame = requestAnimationFrame(step)
      else from.current = target
    }
    frame = requestAnimationFrame(step)
    return () => cancelAnimationFrame(frame)
  }, [target, duration])
  return value
}

// AnimatedText counts up the first number inside an already-formatted
// string ("1,125.4 mi", "78% → 64%") and leaves everything around it alone,
// so callers keep their exact formatting and units.
const leadingNumber = /-?\d[\d,]*(?:\.\d+)?/u
export function AnimatedText({ text }: Readonly<{ text: string }>) {
  const match = leadingNumber.exec(text)
  const raw = match?.[0] ?? ''
  const target = match ? Number(raw.replaceAll(',', '')) : Number.NaN
  const decimals = raw.includes('.') ? (raw.split('.')[1]?.length ?? 0) : 0
  const grouped = raw.includes(',')
  const current = useCountUp(Number.isFinite(target) ? target : 0)
  if (!match || !Number.isFinite(target)) return <>{text}</>
  const shown = grouped
    ? new Intl.NumberFormat('en-US', {
        minimumFractionDigits: decimals,
        maximumFractionDigits: decimals,
      }).format(current)
    : current.toFixed(decimals)
  return (
    <>
      {text.slice(0, match.index)}
      {shown}
      {text.slice(match.index + raw.length)}
    </>
  )
}

export function Sparkline({
  values,
  color = palette.cyan,
  height = 36,
}: Readonly<{ values: readonly number[]; color?: string; height?: number }>) {
  const id = useId()
  const points = values.filter((value) => Number.isFinite(value))
  if (points.length < 2) return null
  const min = Math.min(...points)
  const max = Math.max(...points)
  const span = max - min || 1
  const width = 120
  const coords = points.map((value, index) => [
    (index / (points.length - 1)) * width,
    height - 3 - ((value - min) / span) * (height - 6),
  ])
  const line = coords
    .map(([x, y], index) => `${index === 0 ? 'M' : 'L'}${x?.toFixed(1)},${y?.toFixed(1)}`)
    .join(' ')
  const area = `${line} L${width},${height} L0,${height} Z`
  return (
    <svg
      className="sparkline"
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      aria-hidden="true"
    >
      <defs>
        <linearGradient id={`spark-${id}`} x1="0" x2="0" y1="0" y2="1">
          <stop offset="0%" stopColor={color} stopOpacity={0.35} />
          <stop offset="100%" stopColor={color} stopOpacity={0} />
        </linearGradient>
      </defs>
      <path d={area} fill={`url(#spark-${id})`} />
      <path
        d={line}
        fill="none"
        stroke={color}
        strokeWidth={1.6}
        strokeLinejoin="round"
        strokeLinecap="round"
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  )
}

export function StatTile({
  label,
  value,
  sub,
  icon: Icon,
  tone = 'lime',
  progress,
  trend,
  className,
  animate = true,
}: Readonly<{
  label: ReactNode
  value: string
  sub?: ReactNode
  icon?: LucideIcon | undefined
  tone?: Tone
  /** 0..1 fill for a thin meter under the value. */
  progress?: number | null | undefined
  trend?: readonly number[] | undefined
  className?: string
  /** Off for values that are not quantities (versions, dates). */
  animate?: boolean
}>) {
  const color = palette[tone]
  return (
    <article
      className={`stat-tile ${className ?? ''}`}
      style={{ '--tone': color } as CSSProperties}
    >
      <header>
        {Icon && (
          <span className="stat-icon" aria-hidden="true">
            <Icon />
          </span>
        )}
        <span className="stat-label">{label}</span>
      </header>
      <strong className="stat-value">{animate ? <AnimatedText text={value} /> : value}</strong>
      {sub !== undefined && <small className="stat-sub">{sub}</small>}
      {progress !== undefined && progress !== null && Number.isFinite(progress) && (
        <span className="stat-meter" aria-hidden="true">
          <i style={{ transform: `scaleX(${Math.max(0, Math.min(1, progress))})` }} />
        </span>
      )}
      {trend && trend.length > 1 && <Sparkline values={trend} color={color} />}
    </article>
  )
}

export function StatGrid({
  children,
  className,
}: Readonly<{ children: ReactNode; className?: string }>) {
  return <section className={`stat-grid ${className ?? ''}`}>{children}</section>
}

// RingGauge: the hero battery ring. The arc colour slides from rose at
// empty through amber to lime when full, which reads at a glance without
// a legend. Drawn in SVG so it stays crisp on any display density.
export function RingGauge({
  value,
  size = 220,
  stroke = 14,
  label,
  children,
}: Readonly<{
  value: number | null
  size?: number
  stroke?: number
  label: string
  children?: ReactNode
}>) {
  const id = useId()
  const pct = value === null ? 0 : Math.max(0, Math.min(100, value))
  const animated = useCountUp(pct, 1200)
  const radius = (size - stroke) / 2
  const circumference = 2 * Math.PI * radius
  const sweep = 0.78
  const arc = circumference * sweep
  const filled = (arc * animated) / 100
  const color = pct < 20 ? palette.rose : pct < 45 ? palette.amber : palette.lime
  return (
    <div
      className="ring-gauge"
      style={{ width: size, height: size }}
      role="img"
      aria-label={`${label}: ${value === null ? 'unknown' : `${Math.round(pct)}%`}`}
    >
      <svg viewBox={`0 0 ${size} ${size}`} width={size} height={size}>
        <defs>
          <linearGradient id={`ring-${id}`} x1="0" y1="1" x2="1" y2="0">
            <stop offset="0%" stopColor={color} stopOpacity={0.55} />
            <stop offset="100%" stopColor={color} />
          </linearGradient>
          <filter id={`glow-${id}`} x="-30%" y="-30%" width="160%" height="160%">
            <feGaussianBlur stdDeviation="6" />
          </filter>
        </defs>
        <g transform={`rotate(${90 + (360 * (1 - sweep)) / 2} ${size / 2} ${size / 2})`}>
          <circle
            cx={size / 2}
            cy={size / 2}
            r={radius}
            fill="none"
            stroke="rgba(255,255,255,0.06)"
            strokeWidth={stroke}
            strokeLinecap="round"
            strokeDasharray={`${arc} ${circumference}`}
          />
          <circle
            cx={size / 2}
            cy={size / 2}
            r={radius}
            fill="none"
            stroke={color}
            strokeOpacity={0.45}
            strokeWidth={stroke}
            strokeLinecap="round"
            strokeDasharray={`${filled} ${circumference}`}
            filter={`url(#glow-${id})`}
          />
          <circle
            cx={size / 2}
            cy={size / 2}
            r={radius}
            fill="none"
            stroke={`url(#ring-${id})`}
            strokeWidth={stroke}
            strokeLinecap="round"
            strokeDasharray={`${filled} ${circumference}`}
          />
        </g>
      </svg>
      <div className="ring-center">
        <strong>
          {value === null ? '—' : Math.round(animated)}
          <small>%</small>
        </strong>
        <span>{label}</span>
        {children}
      </div>
    </div>
  )
}

// Gradient <defs> for an area series. Recharts needs the gradient inside the
// chart's own <svg>, so this is rendered as a chart child.
export function AreaGradient({ id, color }: Readonly<{ id: string; color: string }>) {
  return (
    <defs>
      <linearGradient id={id} x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" stopColor={color} stopOpacity={0.38} />
        <stop offset="70%" stopColor={color} stopOpacity={0.06} />
        <stop offset="100%" stopColor={color} stopOpacity={0} />
      </linearGradient>
    </defs>
  )
}

export type DonutSlice = Readonly<{ name: string; value: number; color?: string }>

// Donut: every share-of-total chart in the viewer (AC vs DC, time spent,
// catalog pie panels). The total sits in the hole and the legend lists each
// slice's formatted value and share, so the chart is readable without
// hovering.
export function Donut({
  data,
  format,
  centerLabel,
  height = 240,
}: Readonly<{
  data: readonly DonutSlice[]
  format: (value: number) => string
  centerLabel?: string
  height?: number
}>) {
  const slices = data
    .filter((slice) => Number.isFinite(slice.value) && slice.value > 0)
    .map((slice, index) => ({
      ...slice,
      color: slice.color ?? seriesPalette[index % seriesPalette.length] ?? palette.cyan,
    }))
  const total = slices.reduce((sum, slice) => sum + slice.value, 0)
  if (slices.length === 0) return <p className="no-data">No matching data.</p>
  return (
    <div className="donut">
      <div className="donut-chart" style={{ height }}>
        <ResponsiveContainer width="100%" height="100%">
          <PieChart>
            <Pie
              data={slices}
              dataKey="value"
              nameKey="name"
              innerRadius="68%"
              outerRadius="94%"
              paddingAngle={slices.length > 1 ? 3 : 0}
              cornerRadius={6}
              stroke="none"
              animationDuration={900}
              animationEasing="ease-out"
            >
              {slices.map((slice) => (
                <Cell key={slice.name} fill={slice.color} />
              ))}
            </Pie>
            <Tooltip
              contentStyle={chartTheme.tooltip}
              itemStyle={{ color: '#ececf1' }}
              formatter={(value, name) => [format(Number(value)), String(name)]}
            />
          </PieChart>
        </ResponsiveContainer>
        <div className="donut-center" aria-hidden="true">
          <strong>
            <AnimatedText text={format(total)} />
          </strong>
          <span>{centerLabel ?? 'Total'}</span>
        </div>
      </div>
      <ul className="donut-legend">
        {slices.map((slice) => (
          <li key={slice.name}>
            <i style={{ background: slice.color }} />
            <span>{slice.name}</span>
            <b>{format(slice.value)}</b>
            <em>{total === 0 ? '0' : ((slice.value / total) * 100).toFixed(1)}%</em>
          </li>
        ))}
      </ul>
    </div>
  )
}

export function PageHeader({
  title,
  eyebrow,
  children,
}: Readonly<{ title: ReactNode; eyebrow: ReactNode; children?: ReactNode }>) {
  return (
    <header className="page-head">
      <div>
        <span className="eyebrow">{eyebrow}</span>
        <h1>{title}</h1>
      </div>
      {children !== undefined && <div className="page-head-aside">{children}</div>}
    </header>
  )
}
