# TeslaLog Mini

A tiny, single-binary replacement for the TeslaMate logging pipeline, sized
for a Raspberry Pi Zero 2 W (512MB RAM). It keeps the parts of TeslaMate
worth keeping — Owner API + Tesla streaming, sleep-aware polling, drive and
charge detection — and drops everything else (Postgres, Grafana, MQTT,
Phoenix/Elixir, Docker).

```
Tesla
  │
  ├── Owner API (unofficial, same one TeslaMate uses today)
  └── Tesla streaming websocket (legacy, supplemental GPS/telemetry)
          │
          ▼
Raspberry Pi Zero 2 W
┌─────────────────────────────┐
│ teslalog (one Go binary)     │
│  auth (PKCE/SSO) + refresh   │
│  Owner API client            │
│  streaming client             │
│  vehicle state machine        │
│  SQLite storage (WAL)         │
└──────────────┬───────────────┘
               ▼
     /var/lib/teslalog/tesla.db
```

## Why this exists / what it deliberately does NOT do

This is a **read-only historian**. It does not send commands to the car
(no remote climate/charge control), does not do geofencing or address
lookup, and has no dashboard in v0.1 — just a SQLite file you can query,
export to CSV, or eventually put a tiny web UI on top of.

The one behavior this project cares about getting *right*, more than
any feature, is: **while the car is asleep, leave it alone.** See
[Sleep behavior](#sleep-behavior) below — a badly written Tesla logger
that polls `vehicle_data` every N seconds forever will keep the car
awake and cause real phantom-drain battery loss.

## Repository layout

```
cmd/teslalog/          CLI entrypoint (auth, run, status, wake, backup, export)
internal/config/       TOML config loading (config.example.toml)
internal/tesla/        Owner API + SSO auth + streaming client (isolated on purpose)
internal/vehicle/      sleep-aware state machine (pure, unit-tested, no I/O)
internal/storage/      SQLite schema + queries (ncruces/go-sqlite3, pure Go, no cgo)
internal/backup/       online SQLite backup (safe under WAL) + gzip + rotation
internal/runner/       wires the above into the daemon loop `teslalog run` executes
systemd/teslalog.service
deploy/cross-build.sh  cross-compile for the Pi (linux/arm64)
deploy/install.sh       installs the binary + systemd unit + config on the Pi
```

If Tesla ever kills the Owner API, only `internal/tesla/` needs replacing
(e.g. with a Fleet API client) — the schema, state machine, backups and CLI
are untouched.

## Sleep behavior

The state machine (`internal/vehicle/state_machine.go`) enforces one rule
mechanically:

```
ASLEEP / SUSPENDED  ->  only the cheap GET /api/1/vehicles check
                        (never wakes the car), every suspended_check_interval
                        (default 15m)

ONLINE / DRIVING /
CHARGING / IDLE      ->  full vehicle_data poll every active_interval
                        (default 30s)

IDLE for >= idle_timeout (default 3m)  ->  transition to SUSPENDED,
                        stop calling vehicle_data, let the car sleep
```

`wake_up` is called from exactly one place in the whole codebase: the
manual `teslalog wake` CLI command. The daemon loop (`internal/runner`)
never calls it.

```
                    ASLEEP
                       │ becomes active (seen via cheap check)
                       ▼
                    ONLINE
                  /    |     \
                 ▼     ▼      ▼
           DRIVING  CHARGING  IDLE
               │       │        │ idle_timeout elapsed
               │       │        ▼
               │       │    SUSPENDED
               └───────┴────────┘
                       │
                       ▼
                    ASLEEP (confirmed by next cheap check)
```

## Data model

SQLite tables: `vehicles`, `states` (state-machine history),
`drives` + `positions`, `charging_sessions` + `charging_samples`,
`battery_samples`, `software_updates`. Full schema in
`internal/storage/schema.go`. All distances/speeds are stored in metric
units (km, km/h) even though the Owner API itself reports in miles.

## Building

### Local build (for development/testing on your own machine)

```sh
go build ./...
go test ./...
```

Storage uses `ncruces/go-sqlite3` — SQLite compiled to WebAssembly and
embedded in the Go module, run via the pure-Go `wazero` runtime — so
there's no cgo and no C compiler needed at all, on any platform.

### Cross-compile for the Pi Zero 2 W

On your dev machine (not the Pi), no cross-toolchain required:

```sh
./deploy/cross-build.sh                      # produces teslalog-linux-arm64 (static binary)
```

## Deploying to the Pi

1. Flash 64-bit Raspberry Pi OS Lite, enable SSH + Wi-Fi, boot it.
2. From your dev machine: `scp teslalog-linux-arm64 deploy/ systemd/ config.example.toml pi@<host>:~/teslalog/`
   (or just clone/copy this whole repo to the Pi and cross-build elsewhere,
   copying only the resulting binary in).
3. On the Pi: `cd ~/teslalog && sudo bash deploy/install.sh teslalog-linux-arm64`
4. Authenticate: `sudo -u teslalog teslalog auth -config /etc/teslalog/config.toml`
   - This prints a Tesla login URL. Open it **on any device with a
     browser** (doesn't have to be the Pi), log in, and paste back the
     resulting `auth.tesla.com/void/callback?code=...` URL. Tesla's login
     can require 2FA/CAPTCHA, which is why this can't be fully
     automated — see [Authentication](#authentication).
5. `sudo systemctl enable --now teslalog`
6. `journalctl -u teslalog -f` to watch it discover your vehicle and
   start logging.

## Authentication

Tesla retired the old username/password OAuth grant years ago. Like
TeslaMate, teslapy, and other current tools, `teslalog auth` uses the same
SSO PKCE login flow as the official Tesla mobile app:

1. Generates a PKCE code verifier/challenge pair.
2. Prints a `https://auth.tesla.com/oauth2/v3/authorize?...` URL.
3. You log in in a browser (any device). Tesla redirects to a
   `void/callback` URL that doesn't resolve to anything real — that's
   expected, just copy the URL (or the `code` parameter) from the address
   bar.
4. `teslalog auth` exchanges that code for an access + refresh token pair,
   saved to `/var/lib/teslalog/tokens.json` (mode 0600).
5. The daemon refreshes the access token automatically using the refresh
   token; if the refresh token itself ever expires/is revoked (e.g. Tesla
   password changed), re-run `teslalog auth`.

Your Tesla password is never stored — only these OAuth tokens.

**Caveat:** the Owner API is unofficial. Tesla could restrict or remove it.
That's precisely why `internal/tesla/` is isolated from everything else —
swapping to a different Tesla API surface later doesn't touch your
database, drive detection, or backups.

## Configuration

See `config.example.toml` for every field with inline docs. Key ones:

| Field | Default | Meaning |
|---|---|---|
| `polling.active_interval` | `30s` | `vehicle_data` poll rate while awake |
| `polling.idle_timeout` | `3m` | how long idle-online before we suspend polling |
| `polling.suspended_check_interval` | `15m` | how often we check "is it awake yet" while asleep |
| `backup.retention_days` | `30` | days of nightly backups to keep |

## CLI reference

```
teslalog auth                        interactive Tesla login
teslalog run                         run the daemon (systemd runs this)
teslalog status                      today's drives/distance + last charge
teslalog wake                        explicit manual wake (never automatic)
teslalog backup                      run one backup immediately
teslalog export drives  [-year 2026] [-out drives.csv]
teslalog export charges [-year 2026] [-out charges.csv]
```

## Backups

`internal/backup` uses SQLite's *online backup API* (not a raw file
copy), so it's safe to run against the live database while the daemon is
writing to it under WAL. Backups are gzipped and written to
`/var/lib/teslalog/backups/teslalog-YYYY-MM-DD.db.gz`, pruned after
`retention_days`. The daemon runs this automatically every
`backup.interval` (default 24h); `teslalog backup` runs one on demand.
Consider periodically copying that directory off the Pi's SD card
(rsync/rclone to another machine or cloud storage) — this project
doesn't do off-box replication itself.

## Testing without a real car

`go test ./...` runs:
- `internal/vehicle`: table-driven tests of every state transition and
  the idle-timeout-to-suspend logic, with no network/DB involved.
- `internal/storage`: round-trip tests against a temp SQLite file.
- `internal/runner`: an end-to-end simulation using an `httptest` mock
  Owner API and a mock streaming server that plays back a scripted
  "Drive #1"-style trip (start/end odometer, GPS samples, battery drop)
  and a charging session, asserting the resulting SQLite rows — and
  that no `vehicle_data` calls are made while the simulated car is
  asleep.

This proves the logic end-to-end; it's obviously not a substitute for a
real drive with a real car, which is the actual v0.1 acceptance test (see
below).

## v0.1 acceptance test

Once deployed, take an actual drive and confirm:

```
teslalog export drives -out drives.csv
```

shows a row with sane start/end time, distance, and battery delta, **and**
`journalctl -u teslalog` shows the vehicle going back to `suspended` a few
minutes after you park (not endless `vehicle_data` polling). That's the
whole point of this project.

## What's next (not in v0.1)

- Tiny read-only HTTP dashboard over the SQLite file (`teslalog serve`).
- `teslalog export` as scheduled/automatic CSV/JSON snapshots.
- Optional geofencing / reverse-geocoding for human-readable drive
  start/end locations.
