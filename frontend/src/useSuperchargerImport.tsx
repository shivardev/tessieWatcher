import { useRef, useState, type ChangeEvent, type ReactNode } from 'react'
import { FileSpreadsheet, TriangleAlert, Undo2, X } from 'lucide-react'
import type { ChargeRow } from './domain'
import { getRemoteBackend, runRemoteQuery, runRemoteStatement } from './remoteBackend'
import {
  costUpdateSql,
  parseSuperchargerCsv,
  planImport,
  resultingCost,
  type ConflictChoice,
  type CostUpdate,
  type ImportPlan,
  type ParsedCsv,
  type PlanItem,
} from './superchargerCsv'
import { timestampDate } from './viewSettings'

const when = (date: Date): string =>
  new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(date)
const money = (value: number | null): string => (value === null ? '—' : `$${value.toFixed(2)}`)
const plural = (count: number, word: string): string => `${count} ${word}${count === 1 ? '' : 's'}`

const choiceLabels: Readonly<Record<ConflictChoice, string>> = {
  replace: 'Replace with Tesla price',
  add: 'Add Tesla price to it',
  keep: 'Keep existing cost',
}

type Applied = Readonly<{ fileName: string; previous: readonly CostUpdate[]; written: readonly CostUpdate[] }>

// Charges page control for Tesla's Supercharger history CSV. Reading the
// file is a dry run: every session is shown with what would happen to it,
// charges that already have a cost wait for the user's choice, and only
// "Apply" writes - in one statement, read back afterwards to confirm, with
// an undo that restores the previous costs exactly. Saving needs the data
// server, like the charge cost editor; a local .db file is read-only here.
// Returned as two parts so the page can put the button in its header and
// the review panel below the filters, while they share one state.
export function useSuperchargerImport(charges: readonly ChargeRow[]): Readonly<{
  control: ReactNode
  review: ReactNode
}> {
  const input = useRef<HTMLInputElement>(null)
  const [fileName, setFileName] = useState('')
  const [parsed, setParsed] = useState<ParsedCsv | null>(null)
  const [plan, setPlan] = useState<ImportPlan | null>(null)
  const [included, setIncluded] = useState<ReadonlySet<string>>(new Set())
  const [defaultChoice, setDefaultChoice] = useState<ConflictChoice>('keep')
  const [choices, setChoices] = useState<ReadonlyMap<string, ConflictChoice>>(new Map())
  const [error, setError] = useState<string | null>(null)
  const [result, setResult] = useState<string | null>(null)
  const [applied, setApplied] = useState<Applied | null>(null)
  const [saving, setSaving] = useState(false)
  const backend = getRemoteBackend()

  const showPlan = (next: ImportPlan): void => {
    setPlan(next)
    // Pre-select clear-cut work only: new prices without warnings. Conflicts
    // and anything flagged wait for a deliberate tick.
    setIncluded(
      new Set(next.items.filter((item) => item.status === 'new' && item.warnings.length === 0).map((item) => item.key)),
    )
    setChoices(new Map())
  }

  const read = async (event: ChangeEvent<HTMLInputElement>): Promise<void> => {
    const file = event.target.files?.[0]
    event.target.value = ''
    if (!file) return
    setError(null)
    setResult(null)
    try {
      const contents = parseSuperchargerCsv(await file.text())
      setFileName(file.name)
      setParsed(contents)
      showPlan(planImport(contents, charges))
    } catch (reason: unknown) {
      setPlan(null)
      setError(reason instanceof Error ? reason.message : 'Could not read that file.')
    }
  }

  const choiceFor = (item: PlanItem): ConflictChoice => choices.get(item.key) ?? defaultChoice
  const updates: readonly (CostUpdate & { item: PlanItem })[] = (plan?.items ?? []).flatMap((item) => {
    if (!item.charge || !included.has(item.key)) return []
    const cost = resultingCost(item, choiceFor(item))
    return cost === null ? [] : [{ chargeId: item.charge.id, cost, item }]
  })
  const counts = {
    new: updates.filter((update) => update.item.status === 'new').length,
    replace: updates.filter((update) => update.item.status === 'conflict' && choiceFor(update.item) === 'replace').length,
    add: updates.filter((update) => update.item.status === 'conflict' && choiceFor(update.item) === 'add').length,
  }

  const write = async (target: readonly CostUpdate[]): Promise<readonly number[]> => {
    if (!backend) throw new Error('Not connected to the data server.')
    await runRemoteStatement(backend, costUpdateSql(target))
    // Read the costs back: report anything that did not land as intended.
    const check = await runRemoteQuery(
      backend,
      `SELECT id, cost FROM charging_sessions WHERE id IN (${target.map((update) => update.chargeId).join(',')})`,
    )
    if (check.error) throw new Error(`Saved, but could not verify: ${check.error}`)
    const stored = new Map(check.rows.map((row) => [Number(row[0]), row[1] === null ? null : Number(row[1])]))
    return target
      .filter((update) => {
        const value = stored.get(update.chargeId)
        return update.cost === null ? value !== null : value === null || value === undefined || Math.abs(value - update.cost) >= 0.005
      })
      .map((update) => update.chargeId)
  }

  const apply = async (): Promise<void> => {
    if (!plan || updates.length === 0) return
    setSaving(true)
    setError(null)
    const written = updates.map(({ chargeId, cost }) => ({ chargeId, cost }))
    const previous = updates.map(({ chargeId, item }) => ({ chargeId, cost: item.charge?.cost ?? null }))
    try {
      const failed = await write(written)
      setApplied({ fileName, previous, written })
      setPlan(null)
      setResult(
        failed.length === 0
          ? `Saved and verified ${plural(written.length, 'charge cost')} from ${fileName}.`
          : `Saved, but ${plural(failed.length, 'charge')} did not read back as expected (ids ${failed.join(', ')}). Use Undo to restore the previous costs.`,
      )
    } catch (reason: unknown) {
      setError(`Nothing was changed: ${reason instanceof Error ? reason.message : 'the save failed.'}`)
    } finally {
      setSaving(false)
    }
  }

  const undo = async (): Promise<void> => {
    if (!applied) return
    setSaving(true)
    setError(null)
    try {
      const failed = await write(applied.previous)
      setResult(
        failed.length === 0
          ? `Undone: restored the previous cost of ${plural(applied.previous.length, 'charge')}.`
          : `Undo did not fully apply (ids ${failed.join(', ')}).`,
      )
      if (failed.length === 0) setApplied(null)
    } catch (reason: unknown) {
      setError(reason instanceof Error ? reason.message : 'Undo failed.')
    } finally {
      setSaving(false)
    }
  }

  const toggle = (key: string): void =>
    setIncluded((current) => {
      const next = new Set(current)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })

  const byStatus = (status: PlanItem['status']): number =>
    plan?.items.filter((item) => item.status === status).length ?? 0
  const conflicts = byStatus('conflict')

  const control = (
    <>
      <div className="sc-import-bar">
        <input ref={input} type="file" accept=".csv,text/csv" hidden onChange={(event) => void read(event)} />
        <button
          type="button"
          className="sc-import-button"
          disabled={!backend || saving}
          title={
            backend
              ? 'Read the Supercharger CSV from your Tesla account (Charging > Supercharger > download). You review every change before anything is saved.'
              : 'Connect to your teslalog data server to import Supercharger costs'
          }
          onClick={() => input.current?.click()}
        >
          <FileSpreadsheet aria-hidden="true" /> Import Supercharger CSV
        </button>
        {result && <span className="sc-import-notice" role="status">{result}</span>}
        {applied && !plan && (
          <button type="button" className="sc-import-undo" disabled={saving} onClick={() => void undo()}>
            <Undo2 aria-hidden="true" /> Undo import
          </button>
        )}
        {error && !plan && <span className="sc-import-error" role="alert">{error}</span>}
      </div>

    </>
  )
  const review = (
    <>
      {plan && (
        <section className="sc-import-review" aria-label="Review Supercharger import">
          <header>
            <div>
              <b>Dry run · {fileName}</b>
              <small>Nothing has been saved. Review what would happen, then apply.</small>
            </div>
            <button type="button" className="sc-import-close" aria-label="Close import" onClick={() => setPlan(null)}>
              <X aria-hidden="true" />
            </button>
          </header>

          <div className="sc-import-summary">
            <span className="chip new">{byStatus('new')} new price{byStatus('new') === 1 ? '' : 's'}</span>
            <span className="chip conflict">{conflicts} already priced differently</span>
            <span className="chip same">{byStatus('same')} already correct</span>
            <span className="chip skipped">{byStatus('skipped')} skipped</span>
            {plan.problems.length > 0 && <span className="chip problem">{plural(plan.problems.length, 'unreadable row')}</span>}
            {plan.duplicateRows > 0 && <span className="chip same">{plural(plan.duplicateRows, 'duplicate row')} ignored</span>}
          </div>

          {(plan.problems.length > 0 || plan.notes.length > 0) && (
            <details className="sc-import-details" open={plan.problems.length > 0}>
              <summary>File checks</summary>
              <ul>
                {plan.notes.map((note) => <li key={note}>{note}</li>)}
                {plan.problems.map((problem) => <li key={problem.row}>Row {problem.row}: {problem.message}</li>)}
              </ul>
            </details>
          )}

          <div className="sc-import-options">
            {plan.vins.length > 1 && parsed && (
              <label>
                Car
                <select value={plan.vin ?? ''} onChange={(event) => showPlan(planImport(parsed, charges, event.target.value))}>
                  {plan.vins.map((vin) => <option key={vin} value={vin}>VIN …{vin.slice(-6)}</option>)}
                </select>
              </label>
            )}
            {conflicts > 0 && (
              <label>
                When a charge already has a different cost
                <select value={defaultChoice} onChange={(event) => setDefaultChoice(event.target.value as ConflictChoice)}>
                  {(Object.keys(choiceLabels) as ConflictChoice[]).map((choice) => <option key={choice} value={choice}>{choiceLabels[choice]}</option>)}
                </select>
              </label>
            )}
            <small>
              Matching: a Tesla session belongs to the teslalog charge whose time span contains its start (15 min before to 5 min after),
              DC first, then the nearest. Sessions Tesla split from one plug-in are added together.
            </small>
          </div>

          <div className="table-panel sc-import-table">
            <table>
              <thead>
                <tr>
                  <th aria-label="Include" />
                  <th>Tesla session</th>
                  <th>Supercharger</th>
                  <th>kWh</th>
                  <th>Tesla price</th>
                  <th>teslalog charge</th>
                  <th>Current cost</th>
                  <th>Result</th>
                </tr>
              </thead>
              <tbody>
                {plan.items.map((item) => {
                  const actionable = item.status === 'new' || item.status === 'conflict'
                  const outcome = item.charge && included.has(item.key) ? resultingCost(item, choiceFor(item)) : null
                  return (
                    <tr key={item.key} className={`sc-${item.status}`}>
                      <td>
                        <input
                          type="checkbox"
                          aria-label={`Include ${item.sessions[0]?.site ?? 'session'}`}
                          disabled={!actionable}
                          checked={actionable && included.has(item.key)}
                          onChange={() => toggle(item.key)}
                        />
                      </td>
                      <td>{item.sessions.map((session) => <div key={session.key}>{when(session.start)}</div>)}</td>
                      <td>
                        {item.sessions.map((session) => (
                          <div key={session.key} title={session.fees.join(', ')}>
                            {session.invoiceUrl ? <a href={session.invoiceUrl} target="_blank" rel="noreferrer">{session.site}</a> : session.site}
                          </div>
                        ))}
                        {item.warnings.map((warning) => (
                          <div key={warning} className="sc-warning"><TriangleAlert aria-hidden="true" /> {warning}</div>
                        ))}
                      </td>
                      <td>{item.sessions.reduce((sum, session) => sum + (session.energyKwh ?? 0), 0).toFixed(2)}</td>
                      <td>{money(item.teslaTotal)}</td>
                      <td>
                        {item.charge ? (
                          <>{when(timestampDate(item.charge.time))} <small>{item.charge.type}</small></>
                        ) : (
                          <small>{item.reason}</small>
                        )}
                      </td>
                      <td>{item.charge ? money(item.charge.cost) : '—'}</td>
                      <td>
                        {item.status === 'skipped' ? (
                          <small>Skipped</small>
                        ) : item.status === 'same' ? (
                          <small>Already correct</small>
                        ) : !included.has(item.key) ? (
                          <small>Not included</small>
                        ) : (
                          <>
                            {outcome === null ? <small>Unchanged</small> : <b>→ {money(outcome)}</b>}
                            {item.status === 'conflict' && (
                              <select
                                aria-label="Treatment for this charge"
                                value={choiceFor(item)}
                                onChange={(event) =>
                                  setChoices((current) => new Map(current).set(item.key, event.target.value as ConflictChoice))
                                }
                              >
                                {(Object.keys(choiceLabels) as ConflictChoice[]).map((choice) => (
                                  <option key={choice} value={choice}>{choiceLabels[choice]}</option>
                                ))}
                              </select>
                            )}
                          </>
                        )}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>

          <footer>
            <span>
              {updates.length === 0
                ? 'No changes selected.'
                : `Will set ${plural(updates.length, 'charge cost')}: ${counts.new} new, ${counts.replace} replaced, ${counts.add} added to an existing cost.`}
            </span>
            {error && <span className="sc-import-error" role="alert">{error}</span>}
            <button type="button" className="sc-import-cancel" onClick={() => setPlan(null)}>Cancel</button>
            <button type="button" className="sc-import-apply" disabled={saving || updates.length === 0} onClick={() => void apply()}>
              {saving ? 'Saving…' : `Apply ${plural(updates.length, 'change')}`}
            </button>
          </footer>
        </section>
      )}
    </>
  )
  return { control, review }
}
