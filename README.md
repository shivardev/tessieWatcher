# TeslaLog Mini

A tiny replacement for the TeslaMate logging pipeline, sized for a Raspberry
Pi Zero 2 W (512MB RAM). It keeps the parts of TeslaMate worth keeping —
Owner API + Tesla streaming, sleep-aware polling, drive and charge
detection, and (see [Data model](#data-model--teslamate-parity) below)
essentially the same SQLite data TeslaMate itself records per drive/charge —
and drops everything else (Postgres, Grafana, MQTT, Phoenix/Elixir,
geofencing, address lookup). Point your own Grafana (or anything else) at
the resulting `tesla.db` file whenever you want to look at it; teslalog's
only job is making sure the data is there and correct.

Runs either as a plain systemd service (primary, lowest-overhead path for
the Pi Zero 2 W) or as a Docker container (see
[Running with Docker](#running-with-docker)) — same binary, your choice.

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
internal/portal/       optional read-only HTTP page + database download (see Portal below)
internal/runner/       wires the above into the daemon loop `teslalog run` executes
systemd/teslalog.service
deploy/cross-build.sh  cross-compile for the Pi (linux/arm64)
deploy/install.sh       installs the binary + systemd unit + config on the Pi
Dockerfile, docker-compose.yml   optional container deployment (see below)
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

DRIVING              ->  full vehicle_data poll every driving_interval
                        (default 2.5s, matches TeslaMate's own default)

CHARGING             ->  full vehicle_data poll every charging_interval
                        (default 5s, matches TeslaMate's own default)

ONLINE / IDLE        ->  full vehicle_data poll every online_interval
                        (default 15s, matches TeslaMate's own default)

IDLE for >= idle_timeout (default 3m)  ->  transition to SUSPENDED,
                        stop calling vehicle_data, let the car sleep
```

`wake_up` is called from exactly one place in the whole codebase: the
manual `teslalog wake` CLI command. The daemon loop (`internal/runner`)
never calls it.

```
              ASLEEP / OFFLINE
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
              ASLEEP / OFFLINE (confirmed by next cheap check)
```

(`ASLEEP` vs `OFFLINE` mirrors the Owner API's own vehicle summary state
exactly — "asleep" is a normal sleep, "offline" means the car hasn't
phoned home at all; both stop active polling identically.)

## Data model & TeslaMate parity

SQLite tables: `vehicles`, `states` (state-machine history), `drives` +
`positions`, `charging_sessions` + `charging_samples`, `battery_samples`,
`software_updates`. Full schema in `internal/storage/schema.go`. All
distances/speeds/temperatures are stored in metric units even though the
Owner API itself reports miles/Fahrenheit-adjacent fields.

This schema was built by reading TeslaMate's actual Ecto schema files
(`lib/teslamate/log/{car,position,drive,charging_process,charge,state,
update}.ex`) field-by-field, not from memory, so the parity claim here is
checked against TeslaMate's real source rather than approximate. Per table:

- **positions** (per-drive GPS/telemetry samples): matches TeslaMate's
  `positions` field-for-field — rated/ideal/estimated range as three
  separate figures, raw vs. usable battery %, full climate state (inside/
  outside temp, fan status, driver/passenger temp settings, defrosters,
  climate on/off), all four TPMS tire pressures, battery heater status.
  teslalog additionally keeps `shift_state` and `heading` per position,
  which TeslaMate derives/stores differently.
- **drives**: start/end odometer, distance, duration, battery %, rated
  *and* ideal range, max speed, max/min power, avg outside/inside temp,
  and ascent/descent (computed from position elevation deltas, same as
  TeslaMate's 2025 addition) — all present. TeslaMate normalizes battery
  level via a joined `position_id` rather than storing it directly on
  `drives`; teslalog denormalizes it onto the row itself for a simpler
  single-file schema, with no data loss (the full position history is
  still there).
- **charging_sessions / charging_samples**: matches TeslaMate's
  `charging_processes`/`charges` field-for-field — usable battery %,
  charger phases/pilot current/cable type, fast-charger brand & type,
  rated *and* ideal range, battery heater / "not enough power to heat"
  flags, avg outside temp. Two figures the Owner API doesn't report
  directly (TeslaMate doesn't get them from the API either — it *models*
  them) are opt-in via config: `charge_energy_used_kwh` (estimated from
  a configurable charging-efficiency factor) and `cost` (from a flat
  `price_per_kwh`, vs. TeslaMate's per-geofence pricing). Both are `NULL`
  until you set the corresponding config value.
- **states**: TeslaMate's own `states` table only tracks
  online/offline/asleep; teslalog's is a superset, additionally
  recording driving/charging/idle/suspended (TeslaMate keeps that finer
  state only in memory).
- **vehicles**: VIN, exterior color, wheel type, spoiler type straight
  from the API, plus `model`/`trim_badging`/`marketing_name`. Those
  three are **not** raw API fields even in TeslaMate — the Owner API
  only reports `car_type`/`trim_badging` codes (e.g. `"model3"`,
  `"74D"`); TeslaMate derives the normalized model letter (`"3"`) and
  human trim name (`"LR AWD"`) via a hardcoded lookup table
  (`Vehicle.identify/1`, including a VIN-model-year check to
  disambiguate the Model 3 base trim). `internal/tesla/identity.go`
  ports that exact table (unit-tested against the same cases), so this
  is genuinely the same derived value, not just the same raw string.
  Also carries the optional user-supplied `efficiency_wh_km`
  (config-only, like TeslaMate's — not derived, just stored for
  whatever reads the DB to use).
- **software_updates**: start/end/version, same as TeslaMate.

**Polling cadence**: `driving_interval`/`charging_interval`/
`online_interval` (2.5s/5s/15s) match TeslaMate's own published
defaults (`Vehicle.driving_interval`/`charging_interval`/
`default_interval`), so drive tracks in particular have comparable
position density instead of one flat 30s rate. Deliberately **not**
reproduced: TeslaMate additionally backs its REST poll rate off further
while its streaming connection is actively delivering telemetry,
adaptively tightens/loosens the charging interval via a sample-count
formula, and applies exponential backoff on repeated fetch errors. That
full scheduler is intricate, undocumented outside its source, and tuned
for TeslaMate's own streaming/REST arbitration; teslalog's own streaming
client already supplements REST-derived positions the same way TeslaMate's
does, but the REST cadence itself is a fixed, configurable interval per
state rather than that adaptive state machine.

**Deliberately not ported** (per the original design and your steer that
Grafana/dashboards aren't teslalog's job): TeslaMate's `geofences` and
`addresses` tables (reverse-geocoding drive/charge locations into human
addresses, and per-geofence charge pricing). teslalog stores raw lat/lng
instead — full fidelity, just not resolved to a place name. If you want
that later, it's a self-contained addition (a `geofences` table + a
lookup at drive/charge open/close) that wouldn't touch anything else.

## Building

### Local build (for development/testing on your own machine)

```sh
go build ./...
go test ./...
```

Storage uses `ncruces/go-sqlite3` — SQLite compiled to WebAssembly and
embedded in the Go module, run via the pure-Go `wazero` runtime — so
there's no cgo and no C compiler needed at all, on any platform.

### Cross-compile for x86_64 and the Pi Zero 2 W (arm64)

On your dev machine, no cross-toolchain required for either target
(no cgo anywhere in this project — see above):

```sh
bash deploy/cross-build.sh
# produces:
#   teslalog-linux-amd64      - regular PC/server/VM, e.g. wherever your
#                               existing TeslaMate/Docker host runs, for
#                               testing side-by-side before touching the Pi
#   teslalog-linux-arm64      - the Raspberry Pi Zero 2 W (actual target)
#   teslalog-windows-amd64.exe - for run-teslalog.bat/status-teslalog.bat,
#                               to try it directly on a Windows dev machine
```

Either Linux binary is a single static file — no install step, no
Docker, no systemd required just to run it. Copy it anywhere and:

```sh
cp config.example.toml config.toml   # edit database/token_file paths as you like
./teslalog-linux-amd64 auth   -config config.toml   # one-time interactive login
./teslalog-linux-amd64 run    -config config.toml   # foreground; Ctrl-C to stop
./teslalog-linux-amd64 status -config config.toml
```

### Trying it directly on Windows

No Linux box, Pi, or Docker needed just to see it work: after
`bash deploy/cross-build.sh` produces `teslalog-windows-amd64.exe`, put it
at the repo root (`config.windows-test.toml` is already there, with
relative `database`/`token_file` paths so nothing needs admin rights) and
double-click, or run from a terminal:

```
run-teslalog.bat      # first run: prompts for the one-time Tesla login,
                       # then starts the daemon in the foreground
status-teslalog.bat    # today's drives/last charge, and exports
                       # drives.csv/charges.csv next to the .bat files
```

This is the same daemon and database format as the Linux/Pi path — useful
for a quick first look, or for side-by-side testing against an existing
TeslaMate instance (see below) without leaving your desk. For the real
unattended deployment, follow [Deploying to the Pi](#deploying-to-the-pi)
or [Running with Docker](#running-with-docker) instead.

## Deploying to the Pi

1. Flash 64-bit Raspberry Pi OS Lite, enable SSH + Wi-Fi, boot it. Once
   booted, `ssh` in and run `uname -m`: `aarch64` confirms 64-bit (use
   `teslalog-linux-arm64` below, the usual case); `armv7l` means you
   flashed the 32-bit image instead (use `teslalog-linux-armv7` — same
   steps otherwise).
2. From your dev machine: `scp -r teslalog-linux-arm64 deploy/ systemd/ config.example.toml pi@<host>:~/teslalog/`
   (the `-r` matters — `deploy/` and `systemd/` are directories; without
   it, scp refuses them. Or just clone/copy this whole repo to the Pi and
   cross-build elsewhere, copying only the resulting binary in.)
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

## Running with Docker

Optional — the systemd path above needs no Docker at all. If you'd rather
run it as a container (on the Pi or anywhere else):

```sh
docker compose build
docker compose run --rm teslalog auth      # one-time interactive login (see Authentication below)
docker compose up -d
docker compose logs -f
```

Everything (`tesla.db`, `tokens.json`, backups) lives in the
`teslalog-data` named volume, and `config.toml` lives in a separate
`teslalog-config` volume (seeded from `config.example.toml` on first run),
so both your data and any config edits (VIN, intervals, charging cost)
survive container recreation and image rebuilds. Run any other
subcommand the same way, e.g. `docker compose run --rm teslalog status` or
`docker compose run --rm teslalog export drives -out /var/lib/teslalog/drives.csv`
(then `docker cp` or `docker compose cp` the file out of the volume).

Building directly for the Pi's arm64 from another machine:
`docker buildx build --platform linux/arm64 -t teslalog:arm64 --load .`,
then `docker save`/`docker load` it onto the Pi, or just build it on the
Pi itself (the image is small and the build has no cgo/cross-toolchain
requirement either).

The image is `gcr.io/distroless/static-debian12:nonroot` on top of the
same static, cgo-free binary the systemd path uses — no shell, no package
manager, just the binary and CA certificates for HTTPS to Tesla, running
as an unprivileged uid (matching the non-root posture of
`systemd/teslalog.service` on the bare-metal path).

## Authentication

Tesla retired the old username/password OAuth grant years ago. Like
TeslaMate, teslapy, and other current tools, `teslalog auth` uses the same
SSO PKCE login flow as the official Tesla mobile app:

1. Generates a PKCE code verifier/challenge pair.
2. Prints a `https://auth.tesla.com/oauth2/v3/authorize?...` URL.
3. You log in in a browser. Tesla redirects to
   `tesla://auth/callback?code=...&state=...` — the same custom URL
   scheme the official Tesla app registers itself to handle. Since
   nothing else on a normal machine is registered for it, most browsers
   just hang on a "Loading..." screen forever with nothing visible to
   copy (some show an "Open Tesla?"/"can't open this link" prompt
   instead — behavior varies).
   (Until ~April 2026 this redirected to a dead `void/callback` *https*
   page instead, which every third-party tool — TeslaMate, teslapy,
   tesla_auth, and this project — could just read from the address bar.
   Tesla tightened redirect_uri validation and broke that trick
   account-wide; `tesla://auth/callback` is the fix the wider
   TeslaMate/tesla_auth ecosystem converged on, see
   [teslamate-org/teslamate#5296](https://github.com/teslamate-org/teslamate/issues/5296).)

   Two ways to get the code out of that redirect:
   - **Automatic (recommended, same machine only):** once, run
     `.\deploy\register-tesla-protocol.ps1 -TeslalogPath .\teslalog-windows-amd64.exe`
     (no admin rights needed — it's a per-user registration). From then
     on, Windows hands the `tesla://` redirect straight to
     `teslalog auth-callback`, which relays it to your waiting
     `teslalog auth` automatically — nothing to copy/paste at all. Remove
     it later with `-Unregister`. (Linux/Pi equivalent: register a
     `.desktop` file as the `x-scheme-handler/tesla` handler via
     `xdg-mime` yourself — not scripted here, since the Pi's daemon
     doesn't run a browser at all; do the login step from your own
     desktop instead.)
   - **Manual (works from any device, e.g. logging in from your phone):**
     get the full `tesla://auth/callback?code=...&state=...` URL however
     your browser exposes it — some show it in the failure dialog/page's
     text; if not, open DevTools (F12) *before* clicking login and check
     the Network or Console tab afterwards for the blocked navigation,
     which carries the full URL — and paste it (or just the `code`
     value) into the waiting `teslalog auth` prompt.
4. `teslalog auth` exchanges that code for an access + refresh token pair,
   saved to `/var/lib/teslalog/tokens.json` (mode 0600 — a real
   permission restriction on Linux; on Windows, e.g. via
   `run-teslalog.bat`, the file gets default OS permissions instead, since
   Windows doesn't have a POSIX mode bit for Go's `os.WriteFile` to set).
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
| `polling.driving_interval` | `2.5s` | `vehicle_data` poll rate while driving (matches TeslaMate's default) |
| `polling.charging_interval` | `5s` | `vehicle_data` poll rate while charging (matches TeslaMate's default) |
| `polling.online_interval` | `15s` | `vehicle_data` poll rate while online-idle (matches TeslaMate's default) |
| `polling.idle_timeout` | `3m` | how long idle-online before we suspend polling |
| `polling.suspended_check_interval` | `15m` | how often we check "is it awake yet" while asleep |
| `backup.retention_days` | `30` | days of nightly backups to keep |
| `vehicle.efficiency_wh_km` | `0` (off) | stored on the vehicle row, informational only |
| `charging.efficiency` | `0` (off) | if set, estimates `charge_energy_used_kwh` from energy added |
| `charging.price_per_kwh` | `0` (off) | if set, computes `charging_sessions.cost` |

## CLI reference

```
teslalog auth                        interactive Tesla login
teslalog run                         run the daemon (systemd runs this)
teslalog status                      today's drives/distance + last charge
teslalog wake                        explicit manual wake (never automatic)
teslalog backup                      run one backup immediately
teslalog export drives  [-year 2026] [-out drives.csv]
teslalog export charges [-year 2026] [-out charges.csv]
teslalog update                       self-update to the latest GitHub release
```

(`teslalog auth-callback <url>` also exists, but isn't meant to be run by
hand — see [Authentication](#authentication)'s `register-tesla-protocol.ps1`
step; Windows invokes it for you.)

## Updating

No `git clone`, no rebuilding from source, no re-authenticating:

```sh
sudo systemctl stop teslalog          # optional but recommended: avoids
                                       # replacing the binary while it's
                                       # actively running
sudo -u teslalog teslalog update -config /etc/teslalog/config.toml
sudo systemctl start teslalog
```

`teslalog update` checks this project's
[GitHub Releases](https://github.com/shivardev/tessieWatcher/releases)
for a newer version, downloads the binary matching whatever platform
it's currently running on, and replaces itself in place — same binary
path, same `tokens.json`, same `tesla.db`, nothing else touched. Skipping
the stop/start bracket above still works on Linux (replacing a running
executable's file is safe there — the currently-running process keeps
using the old file until it restarts), it's just not possible on Windows
(the OS locks a running `.exe`); `teslalog update` detects that case and
tells you what to do instead.

First-time install still needs the manual steps in
[Deploying to the Pi](#deploying-to-the-pi) once (there's no
`curl | bash` one-liner yet) — `update` is for every install after that.

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

## Portal (optional web page + database download)

Set `[portal] enabled = true` in config.toml and teslalog serves a tiny
read-only page at `addr` (default `:8083`, e.g.
`http://<pi-hostname-or-ip>:8083`): today's drive count/distance, the
last charge, and a "Download database" button. That button takes a
fresh, consistent snapshot of the live database (same safe online-backup
mechanism as scheduled backups, just uncompressed and on demand) and
downloads it as `tesla-YYYY-MM-DD.db`.

**There is no login.** This is meant for your own home network only —
open that address from your phone/laptop while on the same Wi-Fi as the
Pi. Do not port-forward this address to the public internet; the
database is a complete log of everywhere the vehicle has been and when.

### Viewing the data in Grafana

teslalog doesn't bundle or run Grafana itself (see [Why this exists](#why-this-exists--what-it-deliberately-does-not-do)) —
point your own existing Grafana instance at the downloaded file:

1. Install the community **SQLite datasource plugin**
   (`frser-sqlite-datasource`) on your Grafana instance:
   `grafana-cli plugins install frser-sqlite-datasource`, then restart
   Grafana.
2. Add a data source of that type, pointing its "Path" setting at the
   downloaded `tesla-YYYY-MM-DD.db` file (or, for something that stays
   current, a path you periodically re-download to — this plugin reads
   the file directly, it doesn't poll teslalog itself).
3. Build panels with plain SQL against teslalog's schema (see
   [Data model & TeslaMate parity](#data-model--teslamate-parity) above
   for the exact tables/columns) — e.g. a drives table panel:
   `SELECT start_time, distance_km, duration_min, start_battery_level, end_battery_level, max_speed_kmh FROM drives WHERE status = 'closed' ORDER BY start_time DESC`.

**If you already have TeslaMate's Grafana dashboards**: they won't work
unmodified against this file. TeslaMate's dashboards query a Postgres
database with different table/column names than teslalog's SQLite
schema (even though the underlying data is equivalent — see the parity
table above for the exact mapping) — you'd rebuild the panels' SQL
against teslalog's schema rather than reuse TeslaMate's dashboard JSON
directly.

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

## Verifying against a live TeslaMate instance

Since you already have a working TeslaMate, the easiest way to trust
teslalog's numbers is to run both against the same car at the same time
and compare. This is completely safe — no Docker, systemd, or install
needed, just the plain binary:

1. Build (or grab) `teslalog-linux-amd64` (or `-arm64` if you're running
   this on the Pi/ARM box that hosts TeslaMate) and put it somewhere
   with a `config.toml` next to it (copy `config.example.toml`).
2. `./teslalog-linux-amd64 auth -config config.toml` — do a **fresh**
   interactive login, separate from whatever token TeslaMate is using.
   Tesla's SSO supports multiple simultaneous logins/refresh tokens per
   account, so this doesn't disturb TeslaMate's own session at all.
3. `./teslalog-linux-amd64 run -config config.toml` in a terminal (or
   `tmux`/`screen` if you want it to survive a disconnect) and watch the
   log lines as you drive/charge.
4. Running two independent pollers against one vehicle is safe by
   design: teslalog only ever does read-only `vehicle_data`/vehicle-list
   calls while the car is already awake, and never issues a wake command
   unless you explicitly run `teslalog wake`. It will never fight
   TeslaMate for control of the car's sleep state — it just observes
   whatever state the car (and TeslaMate's own polling) puts it in.
5. After a drive or charge, compare the two datasets:
   ```sh
   ./teslalog-linux-amd64 export drives  -out teslalog_drives.csv
   ./teslalog-linux-amd64 export charges -out teslalog_charges.csv
   sqlite3 config-database-path/tesla.db \
     "select id, start_date, end_date, start_km, end_km, end_km-start_km as distance_km, start_battery_level, end_battery_level from drives order by id desc limit 5;"
   ```
   and check start/end time, distance, and battery-level delta against
   the same drive in TeslaMate's UI (or `psql` against its Postgres DB —
   the `drives`/`charging_processes` tables there use the same
   underlying Owner API fields, just different column names/units in
   places — see [Data model & TeslaMate parity](#data-model--teslamate-parity)
   above for the exact field-by-field mapping). They should match to
   within a poll interval (default 30s) on timestamps, and exactly on
   odometer/battery values since both read the same `vehicle_data`
   fields from Tesla.

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

- ~~Tiny read-only HTTP dashboard over the SQLite file.~~ Done — see
  [Portal](#portal-optional-web-page--database-download) (`[portal]` in
  config.toml, no separate CLI subcommand needed).
- `teslalog export` as scheduled/automatic CSV/JSON snapshots.
- Optional geofencing / reverse-geocoding for human-readable drive
  start/end locations.
