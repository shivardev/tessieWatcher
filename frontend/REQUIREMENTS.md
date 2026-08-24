# teslalog frontend - requirements

Context for whoever (human or AI) picks up the remaining dashboard
implementations. Written by a different AI (Claude) that spent this
session building teslalog's Go daemon, its Grafana dashboards, and two
Postgres bridge tools - not by inspecting this frontend's code, so
treat this as ground truth about the _data source and backend_, not as
a description of what's already been built here.

## What this frontend is

A browser-only, TeslaMate-style dashboard: no backend API to write
against, no server-side rendering. The user downloads a snapshot of
teslalog's live SQLite database (a button already exists for this -
see below) and this app loads it into an in-browser SQLite engine
(sql.js, per this repo's `package.json`) and queries it directly with
the same SQL a server-side app would use. Nothing is ever uploaded
anywhere - the file never leaves the browser.

This coexists with, and does not replace, two other things:

- **teslalog's own Grafana dashboards** (`../grafana/*.json`) - same
  data, same SQL dialect (SQLite), rendered by Grafana instead of this
  app. These are the **primary reference for SQL to port** - see below.
- **`../teslamate-sync` / `../teslamate-import`** - separate tools that
  bridge to/from a real TeslaMate Postgres instance, for people who
  already run TeslaMate's stack. Unrelated to this frontend; no need
  to read those unless you're curious.

## Getting the data

teslalog's built-in HTTP portal (runs on the Pi, LAN-only, no auth)
already serves everything needed:

- `GET /download` - triggers a fresh, consistent snapshot of the live
  SQLite database and streams it back as a file. This is the "Download
  database" button on the portal's own status page. **This is the main
  data source** - fetch it, load the bytes into sql.js, query away.
- `GET /api/status` - small JSON: current state, battery %, odometer,
  active drive/charge id if any. Meant for a frequently-polled live
  header, so a page doesn't have to re-download and re-parse the whole
  database just to show "is it driving right now". Shape:
  ```json
  {
    "vehicle_name": "string",
    "state": "driving|charging|idle|online|asleep|offline|suspended",
    "state_since": "RFC3339 string",
    "battery_level": 72,
    "rated_range_km": 250.5,
    "ideal_range_km": 260.0,
    "odometer_km": 12345.6,
    "firmware": "2026.20.6.11",
    "active_drive_id": 4,
    "active_charge_id": null,
    "updated_at": "RFC3339 string"
  }
  ```
  Every field except `vehicle_name`/`updated_at` is omitted (not
  present, not null) when unknown - check for the key, don't assume
  it's always there.
- `GET /api/meta` - freshness plus change counters:
  ```json
  {
    "last_updated": "RFC3339 string",
    "size_bytes": 10125312,
    "drives": 13,
    "charges": 6,
    "latest_position_id": 38030
  }
  ```
  `drives` and `charges` count only CLOSED rows, so they tick exactly
  when a drive or charge finishes - i.e. when new history becomes
  available. `latest_position_id` moves during an active drive, so it
  works as a liveness signal.

### How to poll without hammering the Pi

**Do not poll `/download` on a timer.** Measured on the real Pi Zero
2 W: `/download` takes ~1s and returns ~10MB, because it makes a fresh
consistent snapshot of the whole database on every request. Once a
minute would be ~14GB/day of transfer *and* the same again in SD-card
writes - to observe data that changes a few times a day. SD write
endurance is the thing that breaks first.

For comparison, on the same hardware: `/api/status` ~80ms and ~250
bytes; `/api/meta` ~50ms and ~100 bytes, touching no disk.

The pattern that works:

1. Poll **`/api/status`** every 30-60s for the live header (state,
   battery, whether a drive/charge is in progress).
2. Poll **`/api/meta`** on the same tick and remember `drives` and
   `charges`.
3. Re-fetch **`/download`** only when one of those counters changes
   (or on first load, or on an explicit user refresh). That is exactly
   the moment there is new completed history to show.

Serving the app itself over HTTP on the LAN and re-downloading a few
times a day is entirely fine. The thing to avoid is a fixed-interval
full-database fetch.

All three already send permissive CORS headers, so they're fetchable
from a page served on a different origin/port (e.g. a Vite dev
server).

**Important deployment caveat**: the portal is plain HTTP, LAN-only, no
TLS. If this app is deployed to GitHub Pages (HTTPS) and tries to
`fetch()` `/api/status`/`/api/meta` from the Pi directly, **browsers
will block it as mixed content** - an HTTPS page cannot fetch a plain
HTTP endpoint by default. If live polling is wanted, this app needs to
run somewhere on the same LAN/HTTP as the Pi, not on GitHub Pages. If
the design is "load a snapshot once, browse offline" (which the
`package.json`/local-file-picker approach suggests), this doesn't
matter and those two endpoints just go unused for now.

## The database schema

This is teslalog's own schema (`../internal/storage/schema.go` is the
literal source of truth - read it directly if anything here is
unclear or seems stale). It is **not** TeslaMate's Postgres schema -
don't confuse the two, they use different table/column names entirely
for the same concepts. Every distance is in **km**, every temperature
in **°C**, regardless of how the SQLite file was produced - unit
conversion (km→mi, °C→°F) is this frontend's job, not the database's.

- **`vehicles`** - one row (usually): `vin`, `display_name`, `model`,
  `trim_badging`, `marketing_name`, `exterior_color`, `wheel_type`,
  `spoiler_type`, `efficiency_wh_km`, `firmware_version`.
- **`states`** - state-machine history: `state` (`asleep`/`offline`/
  `online`/`driving`/`charging`/`idle`/`suspended`), `started_at`,
  `ended_at` (NULL = still current).
- **`drives`** - one row per closed drive: `start_time`/`end_time`,
  `start_odometer_km`/`end_odometer_km`, `distance_km`,
  `duration_min`, `start_battery_level`/`end_battery_level`,
  `start_range_km`/`end_range_km` (rated), `start_ideal_range_km`/
  `end_ideal_range_km`, `start_lat`/`start_lng`/`end_lat`/`end_lng`,
  `start_location`/`end_location` (resolved place name or NULL),
  `max_speed_kmh`, `max_power_kw`, `min_power_kw`,
  `outside_temp_avg_c`/`inside_temp_avg_c`, `ascent_m`/`descent_m`,
  `status` (`'closed'` for anything that should be displayed -
  `'open'` means still in progress, generally filter these out).
- **`positions`** - one row per GPS/telemetry sample during a drive,
  FK'd to `drives.id`. See "Nullability" below before assuming every
  column is always populated - many legitimately aren't, and that's
  correct, not missing data.
- **`charging_sessions`** - one row per closed charging session:
  `start_time`/`end_time`, `start_battery_level`/`end_battery_level`,
  range fields (same shape as drives), `charge_energy_added_kwh`,
  `charge_energy_used_kwh` (estimated), `max_charger_power_kw`,
  `outside_temp_avg_c`, `cost` (NULL if no price configured),
  `latitude`/`longitude`/`location`, `is_dc_fast_charge` (AC vs DC
  split), `status`.
- **`charging_samples`** - one row per sample during a charging
  session, FK'd to `charging_sessions.id`: battery %, charger power/
  voltage/current/phases, `conn_charge_cable`, `fast_charger_present`/
  `fast_charger_brand`/`fast_charger_type`, energy added, range,
  `charge_limit_soc`.
- **`battery_samples`** - opportunistic battery/range snapshots taken
  independent of drives/charges (useful for vampire-drain analysis):
  `battery_level`, `battery_range_km`, `ideal_battery_range_km`,
  `source`.
- **`software_updates`** - `version`, `status`, `start_time`,
  `end_time`.
- **`geocode_cache`** - internal cache, not meaningful to a frontend.

### Nullability - read this before building any chart

Two real bugs were found and fixed _today_ by comparing teslalog's
output against a real TeslaMate instance polling the same real
vehicle - both affect how `positions` data should be interpreted:

1. **Fields the streaming client doesn't report are genuinely NULL,
   not 0/false.** `positions.fan_status`, `driver_temp_setting_c`,
   `passenger_temp_setting_c`, `is_climate_on`,
   `is_rear_defroster_on`/`is_front_defroster_on`,
   `tpms_pressure_fl/fr/rl/rr`, `usable_battery_level`,
   `ideal_range_km`, `est_range_km`, `battery_heater_on`,
   `sentry_mode`, `is_user_present`, `valet_mode` are only reported by
   a REST poll, not by the higher-frequency streaming samples that
   make up roughly 90% of any drive's rows. **Treat NULL in these
   columns as "unknown for this sample", and skip/interpolate rather
   than plot it as a real 0** (e.g. a line chart of fan status should
   have gaps or hold the last-known value, not dip to 0 every other
   sample). `outside_temp_c`/`inside_temp_c` have the same ~10%
   density for the same reason (climate isn't polled every sample) -
   this is expected, not a data-quality bug, and dashboards already
   built (`../grafana/teslalog-drive-details.json`) handle it with
   `spanNulls`/smooth interpolation.
2. **Any `.db` snapshot from before this fix (today's date) has this
   bug baked into its historical drives** - those specific old rows
   have real 0/false where the true value was actually unknown and is
   now unrecoverable (streaming never reported it, so there's nothing
   to backfill). Don't be alarmed if very old drives show suspiciously
   flat fan_status/TPMS lines; new drives after the fix are correct.
3. `elevation_m` is also sparse (~90%) for a related reason: only
   streaming samples carry it, REST-derived ones don't. Same handling
   (gaps, not zeros).

Columns _without_ this caveat - always populated whenever the row
exists at all: `latitude`/`longitude`/`speed_kmh`/`power_kw`/
`odometer_km`/`battery_level`/`range_km` on `positions`; everything on
`drives`/`charging_sessions` (computed once, at close time, from
whatever samples exist); everything on `charging_samples` (charging
has no streaming-vs-REST split - always from a full poll).

## Dashboards to implement

`../grafana/*.json` already has 16 working, already-debugged dashboard
definitions against this exact schema - **port their SQL, don't
re-derive it from scratch**. Each Grafana panel's `targets[].queryText`
is plain SQLite SQL that should mostly work unmodified against sql.js
(Grafana-specific templating like `$variable` substitution is the only
part that doesn't translate directly - swap those for real parameter
binding). Reusing these avoids re-introducing bugs already found and
fixed against real data this session (e.g. Vampire Drain's plausibility
guard against impossible SoC swings from offline/logger-downtime gaps;
`MAX(a,b)` as SQLite's `GREATEST()`; NULL-preserving handling per
above).

Current files, each a real dashboard with its own panels (Overview is
apparently already ported, per earlier progress):

- `teslalog-overview.json` - landing page: vehicle info, lifetime
  odometer/drives/charges, links to the rest.
- `teslalog-drives.json` / `teslalog-drive-stats.json` /
  `teslalog-drive-details.json` - drive list, aggregate stats, and the
  rich per-drive drill-down (route map, speed/power/elevation/
  temperature charts, tire pressures).
- `teslalog-charges.json` / `teslalog-charging-stats.json` - charge
  list and AC/DC/cost/energy aggregates.
- `teslalog-battery.json` / `teslalog-projected-range.json` /
  `teslalog-vampire-drain.json` - battery health, rated range vs
  mileage (degradation trend), and parked-battery-drain analysis.
- `teslalog-efficiency.json` / `teslalog-mileage.json` /
  `teslalog-statistics.json` - derived efficiency figures, cumulative
  odometer, per-month trends.
- `teslalog-locations.json` - most-visited places.
- `teslalog-states.json` / `teslalog-timeline.json` - raw state
  history and a colored-band timeline view of it.
- `teslalog-updates.json` - software update history.

Not worth porting: nothing in this list is TeslaMate-Postgres-specific
(there's no SQLite equivalent of TeslaMate's "Database Information"
dashboard, and there shouldn't be one here).

## Units

The Go portal has its own server-side `metric`/`imperial` toggle (see
`config.toml`'s `portal.units`), but that only affects the portal's own
HTML status page - it has no bearing on this app, which reads raw km/°C
values straight out of the database regardless. Build your own client-
side unit toggle/preference if wanted; there's no signal in the
database itself about the user's preferred units.

## Non-goals for this frontend

- No need to know anything about TeslaMate's Postgres schema, or about
  `../teslamate-sync`/`../teslamate-import` - those are a completely
  separate, opt-in path for people who already run TeslaMate's own
  stack and want its dashboards instead of this app.
- No need to build a Go-side JSON API beyond `/api/status`/`/api/meta`
  (both already exist) - the client-side-SQLite approach is the
  intended architecture, not a stopgap.
- No write path exists or is planned - this is a read-only viewer.
