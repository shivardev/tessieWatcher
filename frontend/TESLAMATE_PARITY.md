# TeslaMate dashboard parity

The implementation target is the dashboard set in TeslaMate's official `grafana/dashboards`
directory and the read-only Grafana instance used during development. The local teslalog Grafana
JSON remains the source of SQLite-compatible queries where it contains the same panel.

## Snapshot constraints

These are data-source constraints, not deferred UI work:

- **Database Information:** PostgreSQL version, shared buffers, database byte size, indexes, and
  `pg_stat_statements` cannot be reconstructed from a SQLite snapshot or a plain dump's data rows.
  The viewer reports snapshot schema, imported row counts, file size, and source metadata instead.
- **Live state:** a GitHub Pages application is HTTPS while a typical TeslaMate LAN endpoint is
  HTTP, so browser mixed-content rules prevent reliable polling. This viewer intentionally displays
  the selected snapshot only.
- **Maps:** route and charging maps use OpenStreetMap tiles. Coordinates remain in the browser, but
  the requested tile area and the viewer's IP address are visible to the tile provider. All database
  and PostgreSQL-dump processing remains local.

## Interaction contract

- Time-series panels expose real timestamp axes and synchronized crosshairs/tooltips.
- Drive telemetry hover also highlights the corresponding route position.
- Drive and charge table rows drill into their detailed telemetry views.
- Distance, speed, elevation, pressure, and temperature are converted at presentation/query time;
  the imported normalized database remains metric.
- Empty or unavailable telemetry is rendered explicitly and is never coerced to zero.
- Dashboard panel placement follows the 24-column Grafana grid on desktop and becomes a single
  readable column on narrow screens.

## Panel-level accounting

Measured directly against TeslaMate's `grafana/dashboards/*.json` (23 dashboards, 166 panels
including its two internal detail views and the Dutch tax report). Every panel below was checked
by rendering it against the live Pi database, not by reading the code.

| TeslaMate dashboard | Its panels | Ported | Notes |
| --- | --- | --- | --- |
| overview | 15 | 15 | |
| drives | 7 | 7 | incl. Incomplete Drives |
| drive-stats | 13 | 13 | its three duplicate Max Speed panels are one card here |
| efficiency | 7 | 7 | direct port |
| trip | 15 | 15 | |
| charges | 7 | 7 | incl. Incomplete Charges |
| charging-stats | 18 | 18 | |
| charge-level | 1 | 2 | split into battery % and rated range |
| battery-health | 11 | 11 | |
| projected-range | 3 | 3 | |
| vampire-drain | 1 | 1 | |
| locations | 9 | 9 | needs a database with the address columns (see below) |
| visited | 4 | 4 | |
| states | 4 | 6 | adds a state-duration breakdown |
| timeline | 1 | 2 | adds time-in-state totals |
| statistics | 1 | 1 | four joined queries collapsed into one; day/week/month/year |
| mileage | 1 | 1 | |
| updates | 3 | 4 | adds current version |
| internal/drive-details | 15 | 17 | adds energy drawn and a GPX export |
| internal/charge-details | 11 | 11 | |
| database-info | 16 | 9 | see below |
| internal/home | 2 | n/a | Grafana's own dashboard list |
| reports/dutch-tax | 1 | 0 | see below |

### Deliberately not ported

- **database-info's PostgreSQL half** (7 panels): server version, shared buffers, relation sizes,
  and the three `pg_stat_statements` panels. teslalog embeds SQLite; there is no server to report
  on. The applicable half - mileage, row counts, firmware, and Incomplete Data - is ported.
- **reports/dutch-tax**: a jurisdiction-specific export of drive rows in a fixed column layout.
  Nothing blocks it; it is simply a report for one country's tax rules. The Drives table already
  carries every column it needs.
- **internal/home**: Grafana's dashboard index. The viewer's own navigation is the equivalent.

### Requires a current teslalog

The Locations dashboard's city/state/country panels read the address components stored alongside
each resolved place name. Databases written before those columns existed show a per-panel
explanation rather than an error, and fill in once the logger has been updated and its backfill
sweep has re-resolved the cached places (a handful of rows, at one per second).
