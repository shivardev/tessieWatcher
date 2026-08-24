# teslalog viewer

A private, static dashboard viewer for teslalog SQLite snapshots and TeslaMate PostgreSQL plain-text dumps. It is built with React, strict TypeScript, Vite, SQL.js, Zod, Leaflet, and Recharts and is suitable for GitHub Pages.

## Privacy model

The selected database or dump is read directly in the browser and is never uploaded. PostgreSQL dumps are streamed into a normalized in-memory SQLite database; private token tables are ignored. Drive Details requests HTTPS map tiles from OpenStreetMap, which discloses the viewed map area and the user's IP address to that tile provider. The database itself is not transmitted.

The application is snapshot-only. It deliberately does not call a Pi portal or TeslaMate API from GitHub Pages; an HTTPS-hosted page cannot reliably poll a LAN HTTP endpoint because browsers block mixed content.

## Supported dashboards

The navigation mirrors the 19 dashboards observed in the reference TeslaMate Grafana instance: Overview, Drives, Drive Stats, Efficiency, Trip, Charges, Charging Stats, Charge Level, Battery Health, Projected Range, Vampire Drain, Locations, Visited, States, Timeline, Statistics, Mileage, Updates, and Database Information. Drive rows open the contextual Drive Details view.

TeslaMate plain dumps using `COPY ... FROM stdin` are converted locally into the viewer's normalized SQLite schema. Custom-format dumps beginning with `PGDMP` are rejected because they require `pg_restore`; create a compatible file with `pg_dump --format=plain --no-owner --no-privileges`. Canonical teslalog panel SQL is generated from `../grafana/teslalog-*.json`. TeslaMate-only surfaces use explicit SQLite equivalents. Battery Health is labeled as a range-based estimate because teslalog does not persist TeslaMate's calculated usable battery capacity.

## Development

```sh
npm install
npm run dev
```

Quality gates:

```sh
npm run verify:queries
npm run typecheck
npm run lint
npm run build
```

`verify:queries` extracts the canonical schema from `../internal/storage/schema.go` and compiles every generated dashboard query against it. `build` regenerates the dashboard catalog automatically.

## GitHub Pages

The Vite base path is relative, so the contents of `dist/` work under a repository subpath. To publish the compiled site to the repository's `gh-pages` branch:

```sh
npm run deploy
```

Then configure the repository's Pages settings to deploy from the `gh-pages` branch. The deploy command runs the complete production build first.

For an artifact-based workflow, configure GitHub Pages to publish a build artifact produced from the `frontend` directory with Node 22 and `npm ci && npm run build`.

The repository's current instruction to keep all frontend changes inside `frontend/` means a root-level `.github/workflows` file is intentionally not created here.
