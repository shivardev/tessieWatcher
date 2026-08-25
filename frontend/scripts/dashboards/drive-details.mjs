// Drive details - port of TeslaMate's grafana/dashboards/internal/drive-details.json
// (15 panels). Reached by clicking a row on the Drives dashboard.
import { buildDashboard } from '../build-dashboard.mjs'

const toUnit = (expression) =>
  `CASE WHEN '$length_unit' = 'mi' THEN (${expression}) / 1.60934 ELSE (${expression}) END`
const drive = `FROM drives WHERE id = $drive_id`

// Power is integrated over the gap to the next sample. TeslaMate caps
// that gap at 1.5 s because it streams positions at roughly 1 Hz;
// teslalog's stream is denser still (measured on the live database:
// 0.31 s mean, 99% under 1.5 s, 5.8 s worst case), so the cap is 15 s
// here - loose enough to keep the 1% of samples in the 1.5-15 s tail,
// tight enough that a break in logging is never counted as time spent
// drawing power. Negative gaps are excluded: timestamps arrive slightly
// out of order on the stream.
const integratedPower = (condition) => `WITH d AS (
  SELECT power_kw,
         (julianday(LEAD(timestamp) OVER (ORDER BY timestamp)) - julianday(timestamp)) * 86400 AS seconds
  FROM positions WHERE drive_id = $drive_id AND power_kw IS NOT NULL
)
SELECT COALESCE(SUM(power_kw * seconds / 3600.0), 0) AS value
FROM d WHERE seconds > 0 AND seconds < 15 AND ${condition}`

await buildDashboard({
  uid: 'teslalog-drive-details',
  title: 'teslalog: Drive details',
  description:
    "Port of TeslaMate's per-drive detail view: the route, the telemetry recorded along it, and the drive's totals.",
  panels: [
    {
      type: 'stat',
      title: 'Distance ($length_unit)',
      w: 6,
      h: 4,
      decimals: 1,
      sql: `SELECT ROUND(${toUnit('distance_km')}, 1) AS value ${drive}`,
    },
    {
      type: 'stat',
      title: 'Drive duration (min)',
      w: 6,
      h: 4,
      decimals: 0,
      sql: `SELECT ROUND(duration_min, 0) AS value ${drive}`,
    },
    {
      type: 'stat',
      title: 'Battery start → end',
      w: 6,
      h: 4,
      sql: `SELECT start_battery_level AS "Start", end_battery_level AS "End" ${drive}`,
    },
    {
      type: 'stat',
      title: 'Max speed ($length_unit/h)',
      w: 6,
      h: 4,
      decimals: 0,
      sql: `SELECT ROUND(${toUnit('max_speed_kmh')}, 0) AS value ${drive}`,
    },
    {
      type: 'stat',
      title: 'Odometer from → to ($length_unit)',
      w: 6,
      h: 4,
      sql: `SELECT ROUND(${toUnit('start_odometer_km')}, 0) || ' → ' || ROUND(${toUnit('end_odometer_km')}, 0) AS value ${drive}`,
    },
    {
      // The MEAN OF THE SPEED SAMPLES, matching TeslaMate's drive-detail
      // panel (avg(speed) over positions in the window) - not distance
      // divided by duration.
      //
      // TeslaMate uses both definitions in different places, on purpose:
      // its Drives *list* computes distance/duration, while its
      // drive-*detail* view averages the samples. They disagree a lot,
      // because samples are not evenly spaced in time - the stream
      // delivers more often when the car is moving - so the sample mean
      // is pulled toward motion. On a real 17-minute, 4.2-mile drive
      // this reads 24 mph where distance/duration reads 14.7.
      //
      // Matching TeslaMate per-panel is the point: the same drive opened
      // side by side has to show the same number. The Drives table keeps
      // distance/duration, which is what TeslaMate's list shows.
      type: 'stat',
      title: 'Ø speed ($length_unit/h)',
      w: 6,
      h: 4,
      decimals: 1,
      sql: `SELECT ROUND(${toUnit('AVG(speed_kmh)')}, 1) AS value
FROM positions WHERE drive_id = $drive_id AND speed_kmh IS NOT NULL`,
    },
    {
      // Net energy: range lost over the drive, priced at the car's Wh/km.
      type: 'stat',
      title: 'Energy consumed (net) (kWh)',
      w: 6,
      h: 4,
      decimals: 2,
      sql: `SELECT ROUND((d.$start_range - d.$end_range) * v.efficiency_wh_km / 1000.0, 2) AS value
FROM drives d JOIN vehicles v ON v.id = d.vehicle_id WHERE d.id = $drive_id`,
    },
    {
      type: 'stat',
      title: 'Consumption (net) (Wh/$length_unit)',
      w: 6,
      h: 4,
      decimals: 0,
      sql: `SELECT ROUND((d.$start_range - d.$end_range) * v.efficiency_wh_km
             / NULLIF(${toUnit('d.distance_km')}, 0), 0) AS value
FROM drives d JOIN vehicles v ON v.id = d.vehicle_id WHERE d.id = $drive_id`,
    },
    {
      // Regeneration: energy put back into the pack, which the API
      // reports as negative power. Sign-flipped so the card reads
      // positive.
      type: 'stat',
      title: 'Energy recovered (kWh)',
      w: 6,
      h: 4,
      decimals: 2,
      sql: integratedPower('power_kw < 0').replace(
        'COALESCE(SUM(power_kw * seconds / 3600.0), 0)',
        'ROUND(COALESCE(SUM(power_kw * seconds / 3600.0), 0) * -1, 2)',
      ),
    },
    {
      type: 'stat',
      title: 'Energy drawn (kWh)',
      w: 6,
      h: 4,
      decimals: 2,
      sql: integratedPower('power_kw > 0').replace(
        'COALESCE(SUM(power_kw * seconds / 3600.0), 0)',
        'ROUND(COALESCE(SUM(power_kw * seconds / 3600.0), 0), 2)',
      ),
    },
    {
      // Total climb and descent, not net elevation change: a round trip
      // over a hill nets to zero but costs real energy in both directions.
      type: 'stat',
      title: 'Elevation up / down',
      w: 12,
      h: 4,
      sql: `WITH steps AS (
  SELECT elevation_m - LAG(elevation_m) OVER (ORDER BY timestamp) AS diff
  FROM positions WHERE drive_id = $drive_id AND elevation_m IS NOT NULL
)
SELECT ROUND(COALESCE(SUM(CASE WHEN diff > 0 THEN diff END), 0)
             * CASE WHEN '$length_unit' = 'mi' THEN 3.28084 ELSE 1 END, 0)
       || ' ↑  ' ||
       ROUND(ABS(COALESCE(SUM(CASE WHEN diff < 0 THEN diff END), 0))
             * CASE WHEN '$length_unit' = 'mi' THEN 3.28084 ELSE 1 END, 0)
       || ' ↓ ' || CASE WHEN '$length_unit' = 'mi' THEN 'ft' ELSE 'm' END AS value
FROM steps`,
    },
    {
      type: 'geomap',
      title: 'Route',
      w: 12,
      h: 12,
      sql: `SELECT latitude, longitude, speed_kmh, timestamp AS time
FROM positions WHERE drive_id = $drive_id AND latitude IS NOT NULL ORDER BY timestamp`,
    },
    {
      type: 'timeseries',
      title: 'Speed & power',
      w: 12,
      h: 12,
      sql: `SELECT timestamp AS time, ${toUnit('speed_kmh')} AS "Speed ($length_unit/h)", power_kw AS "Power (kW)"
FROM positions WHERE drive_id = $drive_id ORDER BY timestamp`,
    },
    {
      type: 'barchart',
      title: 'Speed histogram ($length_unit/h)',
      w: 12,
      h: 8,
      sql: `WITH d AS (
  SELECT ROUND(${toUnit('speed_kmh')} / 10.0) * 10 AS band,
         (julianday(LEAD(timestamp) OVER (ORDER BY timestamp)) - julianday(timestamp)) * 86400 AS seconds
  FROM positions WHERE drive_id = $drive_id AND speed_kmh IS NOT NULL
)
SELECT band AS "Speed", ROUND(SUM(seconds), 0) AS "Seconds"
FROM d WHERE band > 0 AND seconds > 0 AND seconds < 15
GROUP BY band ORDER BY band`,
    },
    {
      type: 'timeseries',
      title: 'Elevation',
      w: 12,
      h: 8,
      sql: `SELECT timestamp AS time,
       (CASE WHEN '$length_unit' = 'mi' THEN elevation_m * 3.28084 ELSE elevation_m END) AS "Elevation"
FROM positions WHERE drive_id = $drive_id AND elevation_m IS NOT NULL ORDER BY timestamp`,
    },
    {
      type: 'timeseries',
      title: 'Temperatures (°$temp_unit)',
      w: 12,
      h: 8,
      sql: `SELECT timestamp AS time,
       (CASE WHEN '$temp_unit' = 'F' THEN outside_temp_c * 9.0 / 5 + 32 ELSE outside_temp_c END) AS "Outside",
       (CASE WHEN '$temp_unit' = 'F' THEN inside_temp_c * 9.0 / 5 + 32 ELSE inside_temp_c END) AS "Inside"
FROM positions WHERE drive_id = $drive_id
  AND (outside_temp_c IS NOT NULL OR inside_temp_c IS NOT NULL) ORDER BY timestamp`,
    },
    {
      type: 'timeseries',
      title: 'Tire pressure',
      w: 12,
      h: 8,
      sql: `SELECT timestamp AS time, tpms_pressure_fl AS "FL", tpms_pressure_fr AS "FR",
       tpms_pressure_rl AS "RL", tpms_pressure_rr AS "RR"
FROM positions WHERE drive_id = $drive_id ORDER BY timestamp`,
    },
  ],
})
