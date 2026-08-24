// Efficiency - a direct port of TeslaMate's grafana/dashboards/efficiency.json
// (7 panels), translated from Postgres to SQLite against teslalog's schema.
//
// Column-name mapping, since teslalog does not repeat "rated" in its
// column names the way TeslaMate does:
//   TeslaMate start_rated_range_km -> teslalog start_range_km
//   TeslaMate start_ideal_range_km -> teslalog start_ideal_range_km
// $start_range/$end_range/$pos_range expand to whichever pair matches
// the viewer's Range type setting (TeslaMate's $preferred_range).
//
// Unit note: TeslaMate's cars.efficiency is kWh/km (0.130), so its
// queries multiply by 1000 to reach Wh. teslalog's efficiency_wh_km is
// already Wh/km (130), so the *1000 is absent here by design, not by
// omission.
import { buildDashboard } from '../build-dashboard.mjs'

const toUnit = (expression) =>
  `CASE WHEN '$length_unit' = 'mi' THEN (${expression}) / 1.60934 ELSE (${expression}) END`
const carEfficiency = `(SELECT efficiency_wh_km FROM vehicles ORDER BY id LIMIT 1)`
const inRange = (column) => `${column} >= datetime('now', '-90 days')`
const closedDrives = `status = 'closed' AND ${inRange('start_time')}`

// The gross-consumption figure walks every range reading in the period in
// time order - drive starts and ends, charge starts and ends - and sums
// each drop between consecutive readings. Unlike the net figure it
// therefore includes range lost while parked (vampire drain) and to
// pre-conditioning, which is what makes it "gross". Shared, with minor
// changes, with the Trip and Charging stats dashboards, exactly as the
// comment in TeslaMate's own copy of this query says.
const grossEnergyWh = `
WITH events AS (
  SELECT 'drive_start' AS event, start_time AS time, $start_range AS range FROM drives WHERE ${closedDrives}
  UNION ALL SELECT 'drive_end', COALESCE(end_time, start_time), $end_range FROM drives WHERE ${closedDrives}
  UNION ALL SELECT 'charge_start', start_time, $start_range FROM charging_sessions WHERE ${closedDrives}
  UNION ALL SELECT 'charge_end', COALESCE(end_time, start_time), $end_range FROM charging_sessions WHERE ${closedDrives}
), ordered AS (
  SELECT event, range, LEAD(range) OVER (ORDER BY time) AS next_range FROM events WHERE range IS NOT NULL
), losses AS (
  SELECT CASE
           WHEN event = 'drive_start' THEN range - next_range
           WHEN range - next_range > 0 THEN range - next_range
           ELSE 0
         END AS loss
  FROM ordered WHERE next_range IS NOT NULL
)
SELECT COALESCE(SUM(loss), 0) * ${carEfficiency} FROM losses`

const loggedDistance = `(SELECT ${toUnit(`SUM(distance_km)`)} FROM drives WHERE ${closedDrives})`

await buildDashboard({
  uid: 'teslalog-efficiency',
  title: 'teslalog: Efficiency',
  description:
    "Port of TeslaMate's Efficiency dashboard. Net consumption counts only range lost while driving; gross also counts range lost while parked. Both follow the Range type (rated/ideal) setting.",
  panels: [
    {
      type: 'stat',
      title: 'Ø Consumption (net) (Wh/$length_unit)',
      w: 8,
      h: 4,
      decimals: 0,
      sql: `SELECT SUM((d.$start_range - d.$end_range) * v.efficiency_wh_km)
       / NULLIF(${toUnit('SUM(d.distance_km)')}, 0) AS value
FROM drives d JOIN vehicles v ON v.id = d.vehicle_id
WHERE d.status = 'closed'
  AND d.distance_km IS NOT NULL
  AND d.$start_range - d.$end_range >= 0.1
  AND ${inRange('d.start_time')}`,
    },
    {
      type: 'stat',
      title: 'Ø Consumption (gross) (Wh/$length_unit)',
      w: 8,
      h: 4,
      decimals: 0,
      sql: `SELECT (${grossEnergyWh}) / NULLIF(${loggedDistance}, 0) AS value`,
    },
    {
      type: 'stat',
      title: 'Logged Distance ($length_unit)',
      w: 8,
      h: 4,
      decimals: 1,
      sql: `SELECT ${loggedDistance} AS value`,
    },
    {
      type: 'stat',
      title: 'Current $preferred_range efficiency (Wh/$length_unit)',
      w: 8,
      h: 4,
      decimals: 0,
      sql: `SELECT efficiency_wh_km * CASE WHEN '$length_unit' = 'mi' THEN 1.60934 ELSE 1 END AS value
FROM vehicles ORDER BY id LIMIT 1`,
    },
    {
      // Buckets drives by outside temperature - 5-degree steps in C, 10 in
      // F, as TeslaMate does - and reports what each bucket cost. Cold
      // weather is the single largest swing in real-world consumption, so
      // this table is the one that explains a month's efficiency.
      type: 'table',
      title: 'Temperature – Driving Efficiency',
      w: 24,
      h: 10,
      sql: `WITH t AS (
  SELECT CASE WHEN '$temp_unit' = 'C'
              THEN ROUND(d.outside_temp_avg_c / 5.0) * 5
              ELSE ROUND((d.outside_temp_avg_c * 9.0 / 5.0 + 32) / 10.0) * 10
         END AS bucket,
         SUM(d.$start_range - d.$end_range) AS total_range,
         SUM(d.distance_km) AS total_distance,
         SUM(d.duration_min) AS duration
  FROM drives d
  WHERE d.status = 'closed'
    AND d.distance_km IS NOT NULL
    AND d.outside_temp_avg_c IS NOT NULL
    AND ${toUnit('d.distance_km')} >= $min_distance
    AND d.$start_range - d.$end_range > 0.1
    AND ${inRange('d.start_time')}
  GROUP BY 1
)
SELECT bucket AS "Outside temp (°$temp_unit)",
       ROUND(total_distance / NULLIF(total_range, 0), 3) AS "Efficiency",
       ROUND(total_range / NULLIF(${toUnit('total_distance')}, 0) * ${carEfficiency}, 0) AS "Consumption (Wh/$length_unit)",
       ROUND(${toUnit('total_distance')}, 1) AS "Driven ($length_unit)",
       ROUND(${toUnit('total_distance')} / NULLIF(duration, 0) * 60, 1) AS "Ø speed ($length_unit/h)"
FROM t
ORDER BY 1 DESC`,
    },
    {
      // The car's real Wh per unit of range, derived from charges rather
      // than configured: energy added divided by range gained. Reported as
      // the three most frequent values with their counts, because a single
      // charge is noisy but the mode over many is the car's true figure.
      // Ideal and rated are separate tables because they are separate
      // ratings, and which one is meaningful depends on the car.
      type: 'table',
      title: 'Derived ideal efficiencies (Wh/$length_unit)',
      w: 12,
      h: 8,
      sql: `SELECT ROUND(charge_energy_added_kwh
              / NULLIF(end_ideal_range_km - start_ideal_range_km, 0)
              / CASE WHEN '$length_unit' = 'mi' THEN 1.0 / 1.60934 ELSE 1 END, 3) * 1000 AS "efficiency_$length_unit",
       COUNT(*) AS "count"
FROM charging_sessions
WHERE status = 'closed'
  AND (julianday(end_time) - julianday(start_time)) * 1440 > 10
  AND end_battery_level <= 95
  AND start_ideal_range_km IS NOT NULL
  AND end_ideal_range_km IS NOT NULL
  AND charge_energy_added_kwh > 0
GROUP BY 1
HAVING "efficiency_$length_unit" IS NOT NULL
ORDER BY 2 DESC
LIMIT 3`,
    },
    {
      type: 'table',
      title: 'Derived rated efficiencies (Wh/$length_unit)',
      w: 12,
      h: 8,
      sql: `SELECT ROUND(charge_energy_added_kwh
              / NULLIF(end_range_km - start_range_km, 0)
              / CASE WHEN '$length_unit' = 'mi' THEN 1.0 / 1.60934 ELSE 1 END, 3) * 1000 AS "efficiency_$length_unit",
       COUNT(*) AS "count"
FROM charging_sessions
WHERE status = 'closed'
  AND (julianday(end_time) - julianday(start_time)) * 1440 > 10
  AND end_battery_level <= 95
  AND start_range_km IS NOT NULL
  AND end_range_km IS NOT NULL
  AND charge_energy_added_kwh > 0
GROUP BY 1
HAVING "efficiency_$length_unit" IS NOT NULL
ORDER BY 2 DESC
LIMIT 3`,
    },
  ],
})
