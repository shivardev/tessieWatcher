// Locations - port of TeslaMate's grafana/dashboards/locations.json (9 panels).
//
// TeslaMate has a normalised addresses table with city/state/country
// columns and foreign keys from drives and charging_processes. teslalog
// stores the resolved display name directly on the drive/charge row and
// keeps the address components in geocode_cache. So the join here is on
// the display name rather than an id: names in geocode_cache are exactly
// the strings written to drives.start_location, and two cache rows that
// share a name are the same place seen from two GPS readings, so they
// necessarily share a city.
//
// A visited place with NO geocode_cache row is a configured geofence -
// geocode.Resolve returns a geofence name before it ever consults the
// cache - which is what makes the Geo-fences panel derivable without a
// geofences table.
import { buildDashboard } from '../build-dashboard.mjs'

const visited = `visited AS (
  SELECT start_location AS name, start_time AS at FROM drives
    WHERE status = 'closed' AND start_location IS NOT NULL AND start_time >= datetime('now', '-90 days')
  UNION ALL
  SELECT end_location, end_time FROM drives
    WHERE status = 'closed' AND end_location IS NOT NULL AND end_time >= datetime('now', '-90 days')
  UNION ALL
  SELECT location, start_time FROM charging_sessions
    WHERE status = 'closed' AND location IS NOT NULL AND start_time >= datetime('now', '-90 days')
)`

// One row per distinct place name. MAX() over the group picks an
// arbitrary non-NULL component, which is correct here because rows
// sharing a name are the same place.
const addresses = `addresses AS (
  SELECT name, MAX(road) AS road, MAX(city) AS city, MAX(county) AS county,
         MAX(state) AS state, MAX(postcode) AS postcode, MAX(country) AS country
  FROM geocode_cache GROUP BY name
)`

const counted = (component) => `WITH ${visited}, ${addresses}
SELECT COUNT(DISTINCT a.${component}) AS value
FROM visited v JOIN addresses a ON a.name = v.name
WHERE a.${component} IS NOT NULL`

const ranked = (component) => `WITH ${visited}, ${addresses}
SELECT a.${component} AS "${component[0].toUpperCase()}${component.slice(1)}", COUNT(*) AS "Visits"
FROM visited v JOIN addresses a ON a.name = v.name
WHERE a.${component} IS NOT NULL
GROUP BY 1 ORDER BY 2 DESC LIMIT 10`

await buildDashboard({
  uid: 'teslalog-locations',
  title: 'teslalog: Locations',
  description:
    "Port of TeslaMate's Locations dashboard. City/state/country come from the address components cached alongside each resolved place name; places with no cached address are configured geofences.",
  panels: [
    {
      type: 'stat',
      title: '# of Addresses',
      w: 6,
      h: 4,
      decimals: 0,
      sql: `WITH ${visited} SELECT COUNT(DISTINCT name) AS value FROM visited`,
    },
    { type: 'stat', title: '# of Cities', w: 6, h: 4, decimals: 0, sql: counted('city') },
    { type: 'stat', title: '# of States', w: 6, h: 4, decimals: 0, sql: counted('state') },
    { type: 'stat', title: '# of Countries', w: 6, h: 4, decimals: 0, sql: counted('country') },
    { type: 'bargauge', title: 'Cities', w: 12, h: 8, sql: ranked('city') },
    { type: 'bargauge', title: 'States', w: 12, h: 8, sql: ranked('state') },
    {
      type: 'table',
      title: 'Last visited',
      w: 12,
      h: 10,
      sql: `WITH ${visited}, ${addresses}
SELECT MAX(v.at) AS "Date", v.name AS "Place", a.city AS "City"
FROM visited v LEFT JOIN addresses a ON a.name = v.name
GROUP BY 2, 3
ORDER BY 1 DESC
LIMIT 100`,
    },
    {
      type: 'table',
      title: 'Addresses',
      w: 12,
      h: 10,
      sql: `WITH ${visited}, ${addresses}
SELECT v.name AS "Name", a.road AS "Road", a.city AS "City", a.county AS "County",
       a.state AS "State", a.postcode AS "Postcode", a.country AS "Country",
       COUNT(*) AS "Visits"
FROM visited v JOIN addresses a ON a.name = v.name
GROUP BY 1, 2, 3, 4, 5, 6, 7
ORDER BY 8 DESC
LIMIT 100`,
    },
    {
      // A place teslalog named without ever reverse-geocoding it can only
      // have come from a [[geofence]] entry in config.toml.
      type: 'table',
      title: 'Geo-fences',
      w: 24,
      h: 8,
      sql: `WITH ${visited}
SELECT v.name AS "Geo-fence", COUNT(*) AS "Visits", MAX(v.at) AS "Last visit"
FROM visited v
WHERE v.name NOT IN (SELECT name FROM geocode_cache)
GROUP BY 1
ORDER BY 2 DESC
LIMIT 100`,
    },
  ],
})
