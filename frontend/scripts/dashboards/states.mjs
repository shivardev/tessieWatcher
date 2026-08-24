// States - port of TeslaMate's grafana/dashboards/states.json (4 panels),
// plus the state-change table teslalog already had.
//
// TeslaMate's state-timeline panel unions drives and charging_processes
// into the states series with numeric codes, because its states table
// records only the API-reported state. teslalog's states table records
// driving and charging as states in their own right, so the timeline is
// a straight read.
import { buildDashboard } from '../build-dashboard.mjs'

await buildDashboard({
  uid: 'teslalog-states',
  title: 'teslalog: States',
  description:
    "Port of TeslaMate's States dashboard: what the car is doing now, when that last changed, and how much of the logged period it has spent parked.",
  panels: [
    {
      type: 'stat',
      title: 'Current State',
      w: 8,
      h: 4,
      sql: `SELECT state AS value FROM states ORDER BY started_at DESC LIMIT 1`,
    },
    {
      type: 'stat',
      title: 'Last state change',
      w: 8,
      h: 4,
      sql: `SELECT started_at AS value FROM states ORDER BY started_at DESC LIMIT 1`,
    },
    {
      // The share of wall-clock time not spent driving. TeslaMate computes
      // 1 - (driving minutes / total elapsed minutes) over the whole
      // logged history, which is why this panel ignores the time range.
      type: 'stat',
      title: 'parked (%)',
      w: 8,
      h: 4,
      decimals: 1,
      sql: `SELECT (1 - SUM(duration_min)
             / NULLIF((julianday(MAX(end_time)) - julianday(MIN(start_time))) * 1440, 0)) * 100 AS value
FROM drives WHERE status = 'closed'`,
    },
    {
      type: 'state-timeline',
      title: 'States',
      w: 24,
      h: 8,
      sql: `SELECT started_at AS time, state AS "State"
FROM states
WHERE started_at >= datetime('now', '-90 days')
ORDER BY started_at`,
    },
    {
      type: 'table',
      title: 'Recent state changes',
      w: 12,
      h: 10,
      sql: `SELECT started_at AS "Time", state AS "State", ended_at AS "Ended",
       ROUND((julianday(COALESCE(ended_at, datetime('now'))) - julianday(started_at)) * 1440, 1) AS "Duration (min)"
FROM states
WHERE started_at >= datetime('now', '-90 days')
ORDER BY started_at DESC
LIMIT 200`,
    },
    {
      // How the logged period breaks down. Answers "is the car actually
      // asleep when I think it is", which is the question that matters for
      // vampire drain and for whether polling is letting it sleep.
      type: 'piechart',
      title: 'Time in each state',
      w: 12,
      h: 10,
      sql: `SELECT state AS "State",
       ROUND(SUM(julianday(COALESCE(ended_at, datetime('now'))) - julianday(started_at)) * 24, 2) AS "Hours"
FROM states
WHERE started_at >= datetime('now', '-90 days')
GROUP BY 1
ORDER BY 2 DESC`,
    },
  ],
})
