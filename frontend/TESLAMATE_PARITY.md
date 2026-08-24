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
