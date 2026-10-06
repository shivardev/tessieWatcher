import { useEffect, useState } from 'react'
import { Check, LoaderCircle, X } from 'lucide-react'

export type LoadingStage = Readonly<{ key: string; label: string }>

export type LoadingState = Readonly<{
  title: string
  source: string
  stages: readonly LoadingStage[]
  done: readonly string[]
  // Stages up to and including this key run one at a time; everything
  // after it runs together, so all of those show as in progress at once.
  sequentialThrough: string
  startedAt: number
  // Shown when the first stage is taking a while, e.g. a server that may
  // be unreachable. Omitted for local files, which are never "waiting".
  slowHint?: string
}>

// After this long on the very first stage, say what is probably happening
// instead of leaving the user to guess.
const slowAfterMs = 6_000

export function LoadingProgress({
  state,
  onCancel,
}: Readonly<{ state: LoadingState; onCancel: () => void }>) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 500)
    return () => clearInterval(timer)
  }, [])

  const done = new Set(state.done)
  const firstPending = state.stages.findIndex((stage) => !done.has(stage.key))
  const parallelFrom = state.stages.findIndex((stage) => stage.key === state.sequentialThrough) + 1
  const isActive = (index: number): boolean =>
    !done.has(state.stages[index]?.key ?? '') &&
    (index === firstPending || (firstPending >= parallelFrom && index >= parallelFrom))
  const elapsed = Math.max(0, Math.floor((now - state.startedAt) / 1000))
  const percent = Math.round((done.size / Math.max(state.stages.length, 1)) * 100)
  const stuckOnFirst =
    state.slowHint !== undefined && done.size === 0 && now - state.startedAt > slowAfterMs

  return (
    <div className="loading-overlay" role="presentation">
      <section className="loading-card" role="status" aria-live="polite" aria-busy="true">
        <header>
          <LoaderCircle className="loading-spin" aria-hidden="true" />
          <div>
            <b>{state.title}</b>
            <small>{state.source}</small>
          </div>
          <span className="loading-elapsed">{elapsed}s</span>
        </header>
        <div className="loading-bar" aria-hidden="true">
          <i style={{ width: `${Math.max(percent, 4)}%` }} />
        </div>
        <ol>
          {state.stages.map((stage, index) => {
            const status = done.has(stage.key) ? 'done' : isActive(index) ? 'active' : 'pending'
            return (
              <li key={stage.key} className={status}>
                {status === 'done' ? (
                  <Check aria-hidden="true" />
                ) : status === 'active' ? (
                  <LoaderCircle className="loading-spin" aria-hidden="true" />
                ) : (
                  <span className="dot" aria-hidden="true" />
                )}
                {stage.label}
                <span className="visually-hidden">
                  {status === 'done' ? ' (done)' : status === 'active' ? ' (in progress)' : ''}
                </span>
              </li>
            )
          })}
        </ol>
        {stuckOnFirst && (
          <p className="loading-slow">{state.slowHint}</p>
        )}
        <footer>
          <span>
            {done.size} of {state.stages.length} loaded
          </span>
          <button type="button" onClick={onCancel}>
            <X aria-hidden="true" /> Cancel
          </button>
        </footer>
      </section>
    </div>
  )
}
