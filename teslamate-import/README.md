# teslamate-import

The reverse of `../teslamate-sync`: brings an existing TeslaMate
installation's history **into** teslalog's own SQLite database. This
is the switching path - for someone moving from TeslaMate to teslalog
who doesn't want to lose years of drive/charge history in the process.

Separate Go module (own `go.mod`), same reasoning as teslamate-sync:
`pgx`/Postgres never becomes a dependency of teslalog's own binary.
Run this once (or occasionally, to top up) on whatever machine can
reach the source Postgres - never on the Pi.

Verified end-to-end against a real, live TeslaMate installation's full
3.5-month history (412 drives, ~1.08M positions, 115 charging
sessions, ~96K charge samples, 540 states, 7 updates) - full import in
under 18 seconds, spot-checked afterward for correctness (fan status,
temperatures, battery levels, ascent/descent, addresses/geofence
names, all present and matching the source).

## Usage

Against a live, already-running TeslaMate:

```bash
go run . -sqlite ./tesla.db -pg-host <host> -pg-user teslamate -pg-pass <password> -pg-db teslamate
```

Against an old `pg_dump` backup with no TeslaMate running anymore
(requires Docker - this spins up a throwaway local Postgres container,
restores the dump into it, imports from that, then removes it):

```bash
go run . -sqlite ./tesla.db -dump ./teslamate-backup.sql
```

`-sqlite` is created (with teslalog's schema) if it doesn't already
exist. Running this against a database teslalog is already actively
logging to is safe - vehicles are upserted by VIN (so imported history
lands on the same vehicle row, not a duplicate), and every other
table's rows get fresh IDs assigned past whatever's already there.

## Why introspection, not a hardcoded column list

TeslaMate's schema has changed across versions (e.g. `ascent`/
`descent` were added to `drives` in a 2025 migration; `drives` itself
has no `start_battery_level`/`end_battery_level` of its own at all -
it's derived via `start_position_id`/`end_position_id` joins into
`positions`, which is exactly what this tool does too). Rather than
hardcode a column list that might `42703` on a schema this tool wasn't
written against, every table is read via `information_schema`
introspection first (see `introspect.go`) - a column this tool expects
but a given install doesn't have degrades to `NULL` instead of failing
the whole import.
