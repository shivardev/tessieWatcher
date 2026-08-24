// Projected range - port of TeslaMate's grafana/dashboards/projected-range.json
// (3 panels). Projected range is the range the car would show at 100%:
// current range scaled up by the usable state of charge. Plotted against
// mileage it shows degradation; against battery level and outdoor
// temperature it shows the two things that make a single reading
// misleading, since both the low end of the pack and the cold suppress
// the estimate without any capacity actually being lost.
import { buildDashboard } from '../build-dashboard.mjs'

const toUnit = (expression) =>
  `CASE WHEN '$length_unit' = 'mi' THEN (${expression}) / 1.60934 ELSE (${expression}) END`

// Only charging samples carry a trustworthy pairing of range and usable
// SOC at rest. Positions are included too, but a sample taken mid-drive
// with heavy draw reads low, so both sources are filtered to readings
// where the reported and usable levels agree - TeslaMate's own
// "sufficiently precise" test.
const samples = `samples AS (
  SELECT p.timestamp, p.$pos_range AS range, p.usable_battery_level AS soc,
         p.odometer_km AS odometer, p.outside_temp_c AS temp
  FROM positions p
  WHERE p.usable_battery_level > 0 AND p.$pos_range > 0
    AND p.battery_level = p.usable_battery_level
    AND p.timestamp >= datetime('now', '-90 days')
  UNION ALL
  SELECT cs.timestamp, cs.$pos_range, cs.usable_battery_level,
         (SELECT MAX(d.end_odometer_km) FROM drives d WHERE d.end_time <= cs.timestamp),
         NULL
  FROM charging_samples cs
  WHERE cs.usable_battery_level > 0 AND cs.$pos_range > 0
    AND cs.battery_level = cs.usable_battery_level
    AND cs.timestamp >= datetime('now', '-90 days')
)`

await buildDashboard({
  uid: 'teslalog-projected-range',
  title: 'teslalog: Projected range',
  description:
    "Port of TeslaMate's Projected range dashboard. Projected range is the range the car would report at 100% SOC; plotting it against mileage, battery level and outdoor temperature separates real degradation from the two effects that merely look like it.",
  panels: [
    {
      type: 'timeseries',
      title: 'Projected Range - Mileage',
      w: 24,
      h: 9,
      sql: `WITH ${samples}
SELECT ROUND(${toUnit('odometer')}, 0) AS "Odometer ($length_unit)",
       ROUND(AVG(${toUnit('range * 100.0 / soc')}), 1) AS "Projected range ($length_unit)"
FROM samples
WHERE odometer IS NOT NULL
GROUP BY 1
ORDER BY 1`,
    },
    {
      type: 'timeseries',
      title: 'Projected Range - Battery Level',
      w: 12,
      h: 9,
      sql: `WITH ${samples}
SELECT soc AS "Battery level (%)",
       ROUND(AVG(${toUnit('range * 100.0 / soc')}), 1) AS "Projected range ($length_unit)"
FROM samples
GROUP BY 1
ORDER BY 1`,
    },
    {
      type: 'timeseries',
      title: 'Projected Range - Outdoor Temp',
      w: 12,
      h: 9,
      sql: `WITH ${samples}
SELECT CASE WHEN '$temp_unit' = 'C' THEN ROUND(temp) ELSE ROUND(temp * 9.0 / 5.0 + 32) END AS "Outside temp (°$temp_unit)",
       ROUND(AVG(${toUnit('range * 100.0 / soc')}), 1) AS "Projected range ($length_unit)"
FROM samples
WHERE temp IS NOT NULL
GROUP BY 1
ORDER BY 1`,
    },
  ],
})
