# teslalog

[![Go Report Card](https://goreportcard.com/badge/github.com/shivardev/tessieWatcher)](https://goreportcard.com/report/github.com/shivardev/tessieWatcher)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](go.mod)
[![Platforms](https://img.shields.io/badge/platforms-linux%2Farm64%20%7C%20linux%2Farmv7%20%7C%20linux%2Famd64%20%7C%20windows-informational)](docs/reference.md#building)

> **Log every drive and charge. Never keep your Tesla awake.**

teslalog is a small, private Tesla data logger. It records your drives,
charging sessions, battery health and trips — the same data
[TeslaMate](https://github.com/teslamate-org/teslamate) collects — but as
**one static binary and one SQLite file**, light enough to run on a
Raspberry Pi Zero 2 W.

No Postgres, no Grafana, no Docker, no Elixir required. Copy a file, log in
once, and it runs.

---

## Why it exists

TeslaMate is excellent, but it is real infrastructure: a database server,
a dashboard server, a web runtime and a message broker. teslalog asks a
simpler question — _what does logging a car actually need?_ — and answers
with three things: a Tesla login, a careful polling loop, and a file to
write rows into.

The project has three promises:

| | Promise | What it means |
|---|---|---|
| 😴 | **Let the car sleep** | It never wakes the car on its own and stops polling when the car is idle, so it causes no phantom battery drain. |
| 🔒 | **Your data stays yours** | Everything lives in a file on your own device. No cloud account, no telemetry, no third-party upload. |
| 🪶 | **Tiny and boring to run** | One binary with no dependencies, sized for a Pi Zero 2 W (512 MB RAM). Updates itself with one command. |

## What you get

- **Automatic logging** of drives (with GPS route, speed, power, elevation,
  temperatures, tyre pressure) and charging sessions (AC/DC, energy, cost).
- **A modern dashboard viewer** — 19 dashboards covering drives, efficiency,
  charging, battery health, projected range, vampire drain, places visited,
  states and more. Dark, fast, and it works on your phone.
- **Real charging costs** — set a price per kWh or per minute for each place
  you charge (home, work, a paid charger), plus session fees and free
  Supercharging.
- **Readable place names** from your own named zones, with optional
  reverse-geocoding.
- **Nightly backups** that are safe to take while logging continues.
- **CSV export** of drives and charges.
- **Optional home-server sync** — mirror everything to PostgreSQL on a PC so
  the Pi only needs to keep the last week.
- **TeslaMate-compatible data** — the schema follows TeslaMate field for
  field, so the numbers can be compared directly.

## How it fits together

```
           Tesla (Owner API + streaming)
                       │
                       ▼
   ┌────────────────────────────────────────┐
   │  teslalog on a Raspberry Pi (or any PC) │
   │  · sleep-aware polling                  │
   │  · writes tesla.db (SQLite)             │
   │  · web viewer on :8083                  │
   └───────────────┬────────────────────────┘
                   │  optional, only when the car is idle
                   ▼
   ┌────────────────────────────────────────┐
   │  Home server (Docker): PostgreSQL       │
   │  full history + the same viewer on :8085│
   └────────────────────────────────────────┘
```

## Quick start (Raspberry Pi)

1. Flash Raspberry Pi OS Lite, enable SSH and Wi-Fi, then on the Pi run:

   ```sh
   curl -fsSL https://raw.githubusercontent.com/shivardev/tessieWatcher/master/deploy/quick-install.sh | sudo bash
   ```

2. Log in to your Tesla account (one time):

   ```sh
   sudo -u teslalog teslalog auth -config /etc/teslalog/config.toml
   ```

   It prints a Tesla login link. Open it on any device, sign in, and paste
   the result back. [Login help →](docs/reference.md#authentication)

3. Start it:

   ```sh
   sudo systemctl enable --now teslalog
   ```

4. Open `http://<your-pi>:8083` from any device on your home network.

Not on a Pi? The same binary runs on any Linux PC or Windows, and there is
a Docker setup too — see [Building](docs/reference.md#building) and
[Running with Docker](docs/reference.md#running-with-docker).

## Looking at your data

| Option | Where | Best for |
|---|---|---|
| **Status page** | `http://<pi>:8083` | Battery, range, today's drives, last charge, one-click database download |
| **Dashboard viewer** | `http://<pi>:8083/app/` | The full set of 19 charts and tables |
| **Home server viewer** | `http://<server>:8085/app/` | Your complete history, after setting up sync |
| **Open a file** | The viewer's _Open database_ button | Exploring a downloaded `.db` file or a TeslaMate `pg_dump`, entirely in your browser |
| **Grafana** | [`grafana/`](grafana/) | Prebuilt Grafana dashboards, if you already use it |
| **CSV** | `teslalog export drives` | Spreadsheets and scripts |

## Everyday commands

```sh
teslalog status          # today's drives and the last charge
teslalog backup          # take a backup now
teslalog export drives   # write drives to CSV  (also: export charges)
teslalog sync            # push pending rows to the home server now
teslalog wake            # wake the car — only ever done when you ask
sudo teslalog update     # update to the latest release
```

Full list and flags: [CLI reference](docs/reference.md#cli-reference).

## Optional: sync to a home server

Keep the Pi small and put the full archive on a PC:

1. On the PC, copy `deploy/postgres.env.example` to `.env`, set the secrets,
   then run `docker compose -f docker-compose.postgres.yml up -d --build`.
2. On the Pi, open `http://<pi>:8083/admin/sync`, enter the server address
   and press **Test, link, and sync now**.

Syncing only happens while the car is idle or asleep. If the server is
offline, nothing is lost — changes wait on the Pi and are sent later.
[Full guide →](docs/reference.md#optional-postgresql-synchronization)

## Privacy & safety

- **Read-only.** teslalog never sends commands to your car (no climate,
  locks or charging control). The only wake call is the manual
  `teslalog wake`.
- **Your password is never stored**, only Tesla's login tokens, in a file
  only the service can read.
- **Keep the status page on your home network.** It has no login, and the
  database is a record of everywhere the car has been. Do not port-forward
  it. If you publish the home server, keep its token on and put it behind
  something like Cloudflare Access.
- **Location lookups are off by default.** Turning on reverse-geocoding
  sends coordinates to a geocoding service, so it is opt-in.

## What it is not

- Not a remote control app — it only watches.
- Not a hosted service — you run it, you own the data.
- Not an official Tesla product. It uses the same unofficial API as
  TeslaMate, which Tesla can change at any time. The Tesla client is kept
  in one isolated package so it can be swapped out without touching your
  data.

## Project layout

```
cmd/teslalog/        the CLI (auth, start, status, sync, export, update…)
internal/tesla/      Tesla login, Owner API and streaming client
internal/vehicle/    the sleep-aware state machine (pure logic, fully tested)
internal/storage/    SQLite schema and queries
internal/runner/     wires everything into `teslalog start`
internal/portal/     status page, JSON API, serves the viewer at /app
internal/cloudsync/  optional Pi → home-server replication
internal/pgserver/   the PostgreSQL-backed server for the home PC
frontend/            the dashboard viewer (React + TypeScript + Vite)
grafana/             optional Grafana dashboards
deploy/              install, cross-build and Docker helpers
```

## Development

```sh
go test ./...                      # backend tests, including a simulated car
cd frontend && npm install
npm run dev                        # viewer at http://localhost:5173
npm test && npm run build          # frontend checks
```

No C compiler needed — SQLite runs as pure Go via WebAssembly, so
cross-compiling for the Pi is just `bash deploy/cross-build.sh`.

## Learn more

- **[Reference manual](docs/reference.md)** — sleep behavior, data model and
  TeslaMate parity, every config option, backups, troubleshooting.
- [Viewer README](frontend/README.md) — how the dashboard app works.
- [Grafana setup](grafana/README.md).
- [Bug log](docs/BUG-LOG.md) — real issues found and how they were fixed.

## Credits

Built on the shoulders of [TeslaMate](https://github.com/teslamate-org/teslamate):
its data model, polling intervals and charging-cost rules are the reference
this project is checked against. If you have the hardware for TeslaMate, it
is a fantastic choice; teslalog is for when you don't.

## License

[MIT](LICENSE)
