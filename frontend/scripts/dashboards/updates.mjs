// Updates - port of TeslaMate's grafana/dashboards/updates.json (3 panels).
//
// TeslaMate reports the MEDIAN gap between updates, not the mean, and the
// distinction matters on a small sample: one long gap over a holiday
// shutdown drags the mean well past anything Tesla actually does. SQLite
// has no percentile function, so the median is taken as the middle row of
// the ordered gaps (the lower of the two middles on an even count, which
// is what percentile_disc(0.5) does too).
import { buildDashboard } from '../build-dashboard.mjs'

const gaps = `gaps AS (
  SELECT (julianday(start_time) - julianday(LAG(start_time) OVER (ORDER BY start_time))) AS days
  FROM software_updates
  WHERE status = 'completed'
)`

await buildDashboard({
  uid: 'teslalog-updates',
  title: 'teslalog: Updates',
  description:
    "Port of TeslaMate's Updates dashboard: every firmware version installed, and how long Tesla typically leaves between them.",
  from: 'now-1y',
  panels: [
    {
      type: 'stat',
      title: 'Updates',
      w: 8,
      h: 4,
      decimals: 0,
      sql: `SELECT COUNT(*) AS value FROM software_updates WHERE status = 'completed'`,
    },
    {
      type: 'stat',
      title: 'Median days between updates',
      w: 8,
      h: 4,
      decimals: 1,
      sql: `WITH ${gaps}, ordered AS (
  SELECT days, ROW_NUMBER() OVER (ORDER BY days) AS rn, COUNT(*) OVER () AS n
  FROM gaps WHERE days IS NOT NULL
)
SELECT days AS value FROM ordered WHERE rn = (n + 1) / 2`,
    },
    {
      type: 'stat',
      title: 'Current version',
      w: 8,
      h: 4,
      sql: `SELECT COALESCE((SELECT firmware_version FROM vehicles ORDER BY id LIMIT 1),
              (SELECT version FROM software_updates WHERE status = 'completed' ORDER BY start_time DESC LIMIT 1)) AS value`,
    },
    {
      type: 'table',
      title: 'Update history',
      w: 24,
      h: 12,
      sql: `SELECT start_time AS "Started", version AS "Version", status AS "Status", end_time AS "Finished",
       ROUND((julianday(end_time) - julianday(start_time)) * 1440, 1) AS "Install (min)",
       ROUND(julianday(start_time) - julianday(LAG(start_time) OVER (ORDER BY start_time)), 1) AS "Days since previous"
FROM software_updates
ORDER BY start_time DESC
LIMIT 200`,
    },
  ],
})
