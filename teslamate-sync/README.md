# teslamate-sync

Optional, opt-in bridge for people who already run TeslaMate's own
Postgres + Grafana stack somewhere (a home server, NAS, always-on PC)
and would rather use TeslaMate's real dashboards - with fan status,
driver/passenger temperature, per-charge cost breakdowns, and the
click-a-date drill-downs - instead of teslalog's own simpler SQLite
dashboards in `../grafana/`.

**Nothing here runs on the Pi, and nothing here is required.** teslalog
itself stays exactly as it always has been: pure Go, SQLite only, no
Postgres, no new dependencies. This tool is a separate module (own
`go.mod`) specifically so `pgx`/Postgres never becomes a dependency of
the binary that actually runs on the Pi - it's meant to run on whatever
machine already hosts your TeslaMate Postgres + Grafana.

## What it does

Every `-interval` (or once, if you leave it unset), this tool:

1. Downloads a fresh snapshot from teslalog's portal (the same
   `/download` endpoint meant for opening the file directly in
   Grafana - see `../internal/portal/portal.go`). No changes needed on
   the Pi side; this is a plain HTTP GET over your LAN.
2. Creates a `teslalog` Postgres database if it doesn't exist yet -
   **never touches a `teslamate` database**, so a real TeslaMate
   instance writing to its own database at the same time is never at
   risk. This database is a disposable sync target, not a source of
   truth - the Pi's SQLite file remains that.
3. Lays out tables matching TeslaMate's own schema closely enough that
   TeslaMate's real, unmodified Grafana dashboard JSON can query it
   without modification.
4. Truncates and rebuilds every table from the latest snapshot - each
   run is a full, idempotent resync, not an incremental one. Given how
   little data an individual car's history is, this is fast (the full
   sync of a 3.5-month/1M-position test dataset took under 10 seconds).

## Running it

Add it as one more service to the **same** docker-compose stack that
already runs your TeslaMate Postgres + Grafana, so it can reach
Postgres by its internal service name instead of needing a published
port:

```yaml
  teslamate-sync:
    build: /path/to/tessieWatcher/teslamate-sync   # or a pushed image, see below
    container_name: teslamate-sync
    restart: unless-stopped
    command:
      - "-portal=http://<pi-ip>:<portal-port>"
      - "-pg-host=database"          # your existing Postgres service's name
      - "-pg-user=teslamate"
      - "-pg-pass=${POSTGRES_PASSWORD}"
      - "-interval=15m"
    depends_on:
      - database
```

Or run it standalone against a published Postgres port, from any
machine on the LAN that can reach both the Pi and Postgres:

```bash
go run . -portal http://<pi-ip>:<portal-port> -pg-host <postgres-host> -pg-pass <password> -interval 15m
```

Flags: `-portal` (required), `-pg-host` (default `localhost`),
`-pg-port` (default `5432`), `-pg-user` (default `teslamate`),
`-pg-pass` (required), `-interval` (omit to sync once and exit).

## Wiring up Grafana

1. **Add a datasource** pointed at the `teslalog` database (Grafana →
   Connections → Data sources → PostgreSQL), same host/credentials as
   your existing TeslaMate datasource, just `Database` = `teslalog`.
2. **Import TeslaMate's real dashboards** against that new datasource:
   in your existing Grafana, open a TeslaMate dashboard (e.g. Drives,
   Charges, Drive Details, Charge Details), use *Export → Export as
   JSON*, then *Import* it back in, picking the new `teslalog`
   datasource when prompted. Repeat per dashboard you want. This reuses
   TeslaMate's actual panels/queries unmodified - nothing here
   reimplements them.

teslalog's own dashboards in `../grafana/` keep working independently
of any of this - see that directory's README. The two are meant to
coexist: teslalog's own dashboards need zero extra infrastructure and
work the moment teslalog is installed; this path is for anyone who
already has TeslaMate's stack running and wants its richer dashboards
pointed at teslalog's data instead.
