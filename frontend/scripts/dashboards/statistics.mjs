// Statistics - port of TeslaMate's grafana/dashboards/statistics.json.
//
// TeslaMate runs four separate queries and lets Grafana join them on the
// period column with a transformation. The viewer's table panel renders
// one result set, so the four are LEFT JOINed here into a single query
// keyed on the period bucket. Same columns, same definitions.
//
// $period is day/week/month/year. SQLite has no date_trunc, so each
// period maps to a strftime format that sorts correctly as text; week
// uses %Y-W%W, which is why the display column is built separately from
// the sort key.
import { buildDashboard } from '../build-dashboard.mjs'

const toUnit = (expression) =>
  `CASE WHEN '$length_unit' = 'mi' THEN (${expression}) / 1.60934 ELSE (${expression}) END`

// The bucket key for a timestamp column, chosen by $period. Written as a
// CASE rather than substituting a format string directly so the same SQL
// is valid in Grafana, where $period is a dashboard variable.
const bucket = (column) => `CASE '$period'
    WHEN 'day'   THEN strftime('%Y-%m-%d', ${column})
    WHEN 'week'  THEN strftime('%Y-W%W', ${column})
    WHEN 'year'  THEN strftime('%Y', ${column})
    ELSE strftime('%Y-%m', ${column})
  END`

await buildDashboard({
  uid: 'teslalog-statistics',
  title: 'teslalog: Statistics',
  description:
    "Port of TeslaMate's Statistics dashboard: driving, charging and consumption totals per day, week, month or year, selected with the Period control.",
  from: 'now-1y',
  panels: [
    {
      type: 'table',
      title: 'Per $period',
      w: 24,
      h: 18,
      sql: `WITH drive_stats AS (
  SELECT ${bucket('d.start_time')} AS period,
         SUM(d.duration_min) AS minutes,
         SUM(d.distance_km) AS distance_km,
         AVG(d.outside_temp_avg_c) AS temp_c,
         COUNT(*) AS drives,
         SUM(d.$start_range - d.$end_range) AS range_diff,
         SUM((d.$start_range - d.$end_range) * v.efficiency_wh_km) AS energy_wh
  FROM drives d JOIN vehicles v ON v.id = d.vehicle_id
  WHERE d.status = 'closed' AND d.start_time >= datetime('now', '-1 year')
  GROUP BY 1
), charge_stats AS (
  SELECT ${bucket('start_time')} AS period,
         COUNT(*) AS charges,
         SUM(charge_energy_added_kwh) AS added_kwh,
         SUM(MAX(COALESCE(charge_energy_added_kwh, 0), COALESCE(charge_energy_used_kwh, 0))) AS used_kwh,
         SUM(cost) AS cost
  FROM charging_sessions
  WHERE status = 'closed' AND start_time >= datetime('now', '-1 year')
    AND (charge_energy_added_kwh IS NULL OR charge_energy_added_kwh > 0)
  GROUP BY 1
), periods AS (
  SELECT period FROM drive_stats UNION SELECT period FROM charge_stats
)
SELECT p.period AS "Period",
       ROUND(d.minutes, 0) AS "Minutes driven",
       ROUND(${toUnit('d.distance_km')}, 1) AS "Driven ($length_unit)",
       ROUND(CASE WHEN '$temp_unit' = 'C' THEN d.temp_c ELSE d.temp_c * 9.0 / 5.0 + 32 END, 1) AS "Ø temp (°$temp_unit)",
       d.drives AS "# of drives",
       ROUND(d.distance_km / NULLIF(d.range_diff, 0) * 100, 0) AS "Efficiency %",
       ROUND(d.energy_wh / NULLIF(${toUnit('d.distance_km')}, 0), 0) AS "Consumption (Wh/$length_unit)",
       c.charges AS "# of charges",
       ROUND(c.added_kwh, 1) AS "Energy added (kWh)",
       ROUND(c.used_kwh, 1) AS "Energy used (kWh)",
       ROUND(c.used_kwh / NULLIF(c.charges, 0), 1) AS "Ø per charge (kWh)",
       ROUND(c.cost, 2) AS "Cost"
FROM periods p
LEFT JOIN drive_stats d ON d.period = p.period
LEFT JOIN charge_stats c ON c.period = p.period
ORDER BY 1 DESC`,
    },
  ],
})
