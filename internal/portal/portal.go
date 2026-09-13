// Package portal is a tiny, read-only HTTP server: one page showing
// today's drives/last charge, a button that downloads a consistent
// snapshot of the live SQLite database (e.g. to open in Grafana via its
// SQLite datasource plugin, or a browser-side frontend that loads it
// into sql.js - teslalog itself has no opinion on what reads the file
// afterward), and two small JSON endpoints (/api/status, /api/meta) for
// exactly that kind of frontend to poll cheaply without re-downloading
// and re-parsing the whole database on every check.
//
// There is deliberately no authentication. This is meant for a trusted
// home LAN only - see config.example.toml's [portal] section and the
// README's Portal section before binding this to anything internet-
// reachable (e.g. a router port-forward), since the served database is a
// complete log of everywhere the vehicle has been and when. Every route
// sets a permissive CORS header for the same reason: a frontend served
// from a different origin/port (e.g. a Vite dev server, or a static
// build served some other way) needs to be able to fetch these directly
// from the browser, and there's no session/cookie-based auth here for a
// wildcard origin to weaken.
package portal

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"teslalog/internal/backup"
	"teslalog/internal/storage"
	"teslalog/internal/webui"
)

// Server serves the portal's routes: "/" (status + download button),
// "/download" (a fresh database snapshot), "/api/status" (cheap live
// status JSON), and "/api/meta" (cheap freshness-check JSON).
type Server struct {
	store     *storage.Store
	dbPath    string
	logs      *LogBuffer
	version   string
	imperial  bool
	snapshots *snapshotCache
}

// New constructs a Server. store is used read-only, for the status line
// on "/"; dbPath is snapshotted fresh on every "/download" request. logs
// is optional (nil is fine, e.g. in tests) - when provided, its recent
// lines render in a "Recent activity" section on "/". units is
// config.PortalConfig.Units ("metric" or "imperial", anything else -
// including "" - behaves as "metric"); it only ever changes how "/"
// displays numbers (mi instead of km) - stored values, /download's
// snapshot, and /api/status's JSON are always km, matching every other
// distance value in the database.
func New(store *storage.Store, dbPath string, logs *LogBuffer, units, version string) *Server {
	return &Server{
		store:     store,
		dbPath:    dbPath,
		logs:      logs,
		imperial:  units == "imperial",
		version:   version,
		snapshots: newSnapshotCache(store, dbPath),
	}
}

const kmPerMi = 1.609344

func (s *Server) distanceUnit() string {
	if s.imperial {
		return "mi"
	}
	return "km"
}

// toDisplayDistance converts a stored km value to the unit the portal
// page should show, given s.imperial.
func (s *Server) toDisplayDistance(km float64) float64 {
	if s.imperial {
		return km / kmPerMi
	}
	return km
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/download", s.handleDownload)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/meta", s.handleAPIMeta)

	// The full browser viewer, embedded in the binary. Served from here
	// rather than only from GitHub Pages because a page served over
	// HTTPS cannot fetch a plain-HTTP LAN address - so only a same-origin
	// copy can actually read /api/meta and /download and stay current.
	// See internal/webui.
	if webui.Available() {
		if app, err := webui.Handler("/app"); err == nil {
			mux.Handle("/app", app)
			mux.Handle("/app/", app)
		} else {
			slog.Warn("portal: could not mount the embedded viewer", "error", err)
		}
	}
	return withCORS(mux)
}

// withCORS wraps h so every route (not just the /api/ ones) is
// fetchable cross-origin - see the package comment for why a wildcard
// is fine here. Preflight OPTIONS requests are answered directly
// rather than reaching the wrapped handler.
func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Run starts the HTTP server on addr and blocks until ctx is canceled,
// then shuts it down gracefully.
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.handler()}

	// Keep the download snapshot pre-built so /download and the embedded
	// viewer's auto-connect serve instantly instead of building on demand.
	// A 60s tick matches the viewer's own /api/meta poll interval, so a
	// finished drive is snapshotted before the viewer next asks for it,
	// and an idle car costs only a cheap signature check per tick.
	go s.snapshots.refresh(ctx, 60*time.Second)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("portal shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("portal server: %w", err)
		}
		return nil
	}
}

type indexData struct {
	VehicleName string
	// DistanceUnit is "km" or "mi" (see Server.imperial) - every *Km-
	// suffixed field below is already converted to whichever this is
	// by the time it lands in this struct; the field names keep the
	// Km suffix because that's what they're computed from, not
	// necessarily what they're displayed as.
	DistanceUnit   string
	HasState       bool
	CurrentState   string
	StateSince     string
	TodayDrives    int
	TodayKm        float64
	HasLastCharge  bool
	LastChargeFrom int
	LastChargeTo   int
	LastChargeKwh  float64
	LastChargeEnd  string
	LastChargeLoc  string
	// HasBattery/BatteryLevel/RatedRangeKm/IdealRangeKm/BatteryAt come
	// from storage.Store.LatestBatteryReading - see there for which
	// table it's sourced from and why. IdealRangeKm is Tesla's older,
	// often-frozen range figure, shown alongside the "rated" one the
	// same way TeslaMate's own vehicle status card does.
	HasBattery   bool
	BatteryLevel int
	RatedRangeKm float64
	IdealRangeKm float64
	BatteryAt    string
	Firmware     string
	Version      string
	HasLifetime  bool
	OdometerKm   float64
	TotalDrives  int
	TotalKm      float64
	TotalCharges int
	TotalKwh     float64
	// HasSleepStats/AsleepPct24h - see storage.SleepStats.AsleepPct's
	// doc comment. Concrete proof the daemon's "never wake a sleeping
	// car" design goal (see README's Sleep behavior section) is
	// actually working, not just a policy taken on faith. Shown as
	// soon as there's at least one recorded state - deliberately not
	// gated behind HasLifetime, since it's most reassuring to see in
	// the very first day, before any drives have happened yet.
	HasSleepStats bool
	AsleepPct24h  float64
	// HasViewer gates the link to the embedded browser viewer: a build
	// whose frontend assets were never compiled should not advertise a
	// page that would 404. See internal/webui.
	HasViewer     bool
	RecentDrives  []recentDrive
	RecentCharges []recentCharge
	LogLines      []string
}

type recentDrive struct {
	StartTime       string
	FromLoc         string
	ToLoc           string
	DistanceKm      float64
	DurationMin     float64
	StartBattery    int
	EndBattery      int
	EfficiencyRatio float64
}

type recentCharge struct {
	StartTime      string
	Location       string
	StartBattery   int
	EndBattery     int
	EnergyAddedKwh float64
	ChargeType     string
	MaxPowerKw     float64
	Cost           float64
}

// stateBadgeClass buckets a raw states.state value into one of three CSS
// classes so the current-state badge reads at a glance: green while the
// car is doing something worth noticing (driving/charging), gray while
// it's correctly left alone asleep/suspended (that's success, not idle
// inactivity - see README's Sleep behavior section), amber otherwise.
func stateBadgeClass(state string) string {
	switch state {
	case "driving", "charging":
		return "state-active"
	case "asleep", "offline", "suspended":
		return "state-asleep"
	default:
		return "state-online"
	}
}

var indexTemplate = template.Must(template.New("index").Funcs(template.FuncMap{
	"stateBadgeClass": stateBadgeClass,
}).Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="15">
<title>{{.VehicleName}} · teslalog</title>
<style>
  :root {
    --bg: #0b0d10; --panel: #14171c; --panel-2: #1b1f26; --border: #262b33;
    --text: #e6e8eb; --text-dim: #8b93a1; --accent: #3ea6ff;
    --green-bg: #103b23; --green-fg: #4ade80;
    --amber-bg: #3a2c0d; --amber-fg: #fbbf24;
    --gray-bg: #23262c; --gray-fg: #9aa3af;
  }
  * { box-sizing: border-box; }
  body {
    font-family: -apple-system, system-ui, "Segoe UI", Roboto, sans-serif;
    max-width: 42rem; margin: 2.5rem auto; padding: 0 1.25rem;
    background: var(--bg); color: var(--text); line-height: 1.5;
  }
  h1 { font-size: 1.35rem; font-weight: 600; margin: 0 0 0.25rem; }
  .subtitle { color: var(--text-dim); font-size: 0.85rem; margin-bottom: 1.25rem; }
  .warn {
    background: var(--amber-bg); color: var(--amber-fg); border: 1px solid #5c4a12;
    border-radius: 8px; padding: 0.75rem 1rem; margin-bottom: 1.25rem; font-size: 0.85rem;
  }
  .card {
    background: var(--panel); border: 1px solid var(--border); border-radius: 10px;
    padding: 1rem 1.25rem; margin-bottom: 1rem;
  }
  .stat { display: flex; justify-content: space-between; align-items: center; padding: 0.35rem 0; }
  .stat + .stat { border-top: 1px solid var(--border); }
  .stat .label { color: var(--text-dim); font-size: 0.9rem; }
  .stat .value { font-weight: 600; }
  .badge {
    display: inline-block; padding: 0.2rem 0.75rem; border-radius: 999px;
    font-weight: 600; text-transform: capitalize; font-size: 0.85rem;
  }
  .state-active { background: var(--green-bg); color: var(--green-fg); }
  .state-asleep { background: var(--gray-bg); color: var(--gray-fg); }
  .state-online { background: var(--amber-bg); color: var(--amber-fg); }
  button {
    font-size: 1rem; padding: 0.65rem 1.4rem; border-radius: 8px; border: none;
    background: var(--accent); color: #04121f; cursor: pointer; font-weight: 600;
    width: 100%;
  }
  button:hover { background: #5fb8ff; }
  form { margin: 0; flex: 1; }
  .actions { display: flex; gap: 0.6rem; margin: 1.25rem 0; flex-wrap: wrap; }
  .actions > * { flex: 1 1 12rem; }
  a.button {
    display: block; text-align: center; text-decoration: none;
    font-size: 1rem; padding: 0.65rem 1.4rem; border-radius: 8px;
    border: 1px solid var(--border); background: transparent;
    color: var(--text); cursor: pointer; font-weight: 600;
  }
  a.button.primary { background: var(--accent); border-color: var(--accent); color: #04121f; }
  a.button.primary:hover { background: #5fb8ff; }
  h2 { font-size: 0.95rem; font-weight: 600; color: var(--text-dim); text-transform: uppercase; letter-spacing: 0.03em; margin: 1.75rem 0 0.6rem; }
  table { width: 100%; border-collapse: collapse; font-size: 0.85rem; }
  table th, table td { text-align: left; padding: 0.45rem 0.4rem; border-bottom: 1px solid var(--border); }
  table th { color: var(--text-dim); font-weight: 500; font-size: 0.75rem; text-transform: uppercase; }
  table td.num { text-align: right; font-variant-numeric: tabular-nums; }
  pre.log {
    background: #05070a; color: #b8c0cc; font-size: 0.78rem; line-height: 1.45;
    padding: 0.85rem; border-radius: 8px; max-height: 16rem; overflow-y: auto;
    white-space: pre-wrap; word-break: break-all; border: 1px solid var(--border);
  }
  .footer { color: var(--text-dim); font-size: 0.78rem; margin-top: 1.5rem; text-align: center; }
  a { color: var(--accent); }
</style>
</head>
<body>
  <h1>{{.VehicleName}}</h1>
  <div class="subtitle">teslalog{{if .Version}} v{{.Version}}{{end}}{{if .Firmware}} &middot; firmware {{.Firmware}}{{end}}</div>
  <div class="warn">This page has no login - only reachable on your local network. Anyone on this Wi-Fi/LAN can see this page and download the database.</div>

  <div class="card">
    <div class="stat">
      <span class="label">Current state</span>
      <span class="value">
        {{if .HasState}}<span class="badge {{stateBadgeClass .CurrentState}}">{{.CurrentState}}</span>{{else}}<span style="color:var(--text-dim)">not seen yet</span>{{end}}
      </span>
    </div>
    {{if .HasState}}<div class="stat"><span class="label">Since</span><span class="value">{{.StateSince}}</span></div>{{end}}
    {{if .HasSleepStats}}<div class="stat"><span class="label">Asleep (last 24h)</span><span class="value">{{printf "%.0f" .AsleepPct24h}}%</span></div>{{end}}
    {{if .HasBattery}}
    <div class="stat"><span class="label">Battery</span><span class="value">{{.BatteryLevel}}%</span></div>
    <div class="stat"><span class="label">Rated range</span><span class="value">{{printf "%.0f" .RatedRangeKm}} {{.DistanceUnit}}{{if .IdealRangeKm}} <span style="color:var(--text-dim)">({{printf "%.0f" .IdealRangeKm}} {{.DistanceUnit}} ideal)</span>{{end}}</span></div>
    {{end}}
    <div class="stat"><span class="label">Drives today</span><span class="value">{{.TodayDrives}}</span></div>
    <div class="stat"><span class="label">Distance today</span><span class="value">{{printf "%.1f" .TodayKm}} {{.DistanceUnit}}</span></div>
    {{if .HasLastCharge}}
    <div class="stat">
      <span class="label">Last charge</span>
      <span class="value">{{.LastChargeFrom}}% &rarr; {{.LastChargeTo}}% &middot; {{printf "%.1f" .LastChargeKwh}} kWh{{if .LastChargeLoc}} &middot; {{.LastChargeLoc}}{{end}}</span>
    </div>
    {{end}}
  </div>

  <div class="actions">
    {{if .HasViewer}}
    <a class="button primary" href="/app/">📊 Open the dashboards</a>
    {{end}}
    <form action="/download" method="get">
      <button type="submit">⬇ Download database (tesla.db)</button>
    </form>
  </div>

  {{if .HasLifetime}}
  <div class="card">
    <div class="stat"><span class="label">Lifetime odometer</span><span class="value">{{printf "%.0f" .OdometerKm}} {{.DistanceUnit}}</span></div>
    <div class="stat"><span class="label">Lifetime drives</span><span class="value">{{.TotalDrives}} &middot; {{printf "%.0f" .TotalKm}} {{.DistanceUnit}}</span></div>
    <div class="stat"><span class="label">Lifetime charging</span><span class="value">{{.TotalCharges}} &middot; {{printf "%.0f" .TotalKwh}} kWh</span></div>
  </div>
  {{end}}

  {{if .RecentDrives}}
  <h2>Recent drives</h2>
  <div class="card" style="padding:0.5rem 1rem; overflow-x:auto;">
    <table>
      <tr><th>When</th><th>From</th><th>To</th><th class="num">{{.DistanceUnit}}</th><th class="num">min</th><th class="num">%</th><th class="num">eff.</th></tr>
      {{range .RecentDrives}}
      <tr>
        <td>{{.StartTime}}</td>
        <td>{{if .FromLoc}}{{.FromLoc}}{{else}}&mdash;{{end}}</td>
        <td>{{if .ToLoc}}{{.ToLoc}}{{else}}&mdash;{{end}}</td>
        <td class="num">{{printf "%.1f" .DistanceKm}}</td>
        <td class="num">{{printf "%.0f" .DurationMin}}</td>
        <td class="num">{{.StartBattery}}&rarr;{{.EndBattery}}</td>
        <td class="num">{{if .EfficiencyRatio}}{{printf "%.2f" .EfficiencyRatio}}{{else}}&mdash;{{end}}</td>
      </tr>
      {{end}}
    </table>
  </div>
  {{end}}

  {{if .RecentCharges}}
  <h2>Recent charges</h2>
  <div class="card" style="padding:0.5rem 1rem; overflow-x:auto;">
    <table>
      <tr><th>When</th><th>Where</th><th>Type</th><th class="num">%</th><th class="num">kWh</th><th class="num">max kW</th><th class="num">cost</th></tr>
      {{range .RecentCharges}}
      <tr>
        <td>{{.StartTime}}</td>
        <td>{{if .Location}}{{.Location}}{{else}}&mdash;{{end}}</td>
        <td>{{.ChargeType}}</td>
        <td class="num">{{.StartBattery}}&rarr;{{.EndBattery}}</td>
        <td class="num">{{printf "%.1f" .EnergyAddedKwh}}</td>
        <td class="num">{{printf "%.1f" .MaxPowerKw}}</td>
        <td class="num">{{if .Cost}}{{printf "%.2f" .Cost}}{{else}}&mdash;{{end}}</td>
      </tr>
      {{end}}
    </table>
  </div>
  {{end}}

  {{if .LogLines}}
  <h2>Recent activity</h2>
  <pre class="log">{{range .LogLines}}{{.}}
{{end}}</pre>
  {{end}}

  <p class="footer">This page refreshes itself every 15s.</p>
</body>
</html>
`))

// apiStatus is handleAPIStatus's response shape: the same "what's
// going on right now" facts handleIndex renders as HTML, as JSON
// instead - meant to be polled often (every few seconds) by a live
// header/badge, without the cost of downloading and re-parsing the
// whole database just to answer "is it driving right now".
type apiStatus struct {
	VehicleName    string   `json:"vehicle_name"`
	State          string   `json:"state,omitempty"`
	StateSince     string   `json:"state_since,omitempty"`
	BatteryLevel   *int     `json:"battery_level,omitempty"`
	RatedRangeKm   *float64 `json:"rated_range_km,omitempty"`
	IdealRangeKm   *float64 `json:"ideal_range_km,omitempty"`
	OdometerKm     *float64 `json:"odometer_km,omitempty"`
	Firmware       string   `json:"firmware,omitempty"`
	ActiveDriveID  *int64   `json:"active_drive_id,omitempty"`
	ActiveChargeID *int64   `json:"active_charge_id,omitempty"`
	// Version is the running teslalog build, so "did my update
	// actually land?" is answerable without SSH-ing in or inferring
	// it from the database's schema shape.
	Version   string `json:"version,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	out := apiStatus{VehicleName: "Vehicle", Version: s.version, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}

	var vehicleID int64
	var displayName, firmware, model, marketingName string
	row := s.store.DB().QueryRow(`
		SELECT id, COALESCE(display_name, ''), COALESCE(firmware_version, ''),
		       COALESCE(model, ''), COALESCE(marketing_name, '')
		FROM vehicles ORDER BY id LIMIT 1`)
	if err := row.Scan(&vehicleID, &displayName, &firmware, &model, &marketingName); err != nil {
		if err != sql.ErrNoRows {
			slog.Warn("portal: api/status query vehicle failed", "error", err)
		}
		writeJSON(w, out)
		return
	}
	out.VehicleName = vehicleDisplayName(displayName, model, marketingName)
	out.Firmware = firmware

	if state, err := s.store.CurrentState(vehicleID); err == nil && state != "" {
		out.State = state
	}
	var stateSince string
	if err := s.store.DB().QueryRow(`SELECT started_at FROM states WHERE vehicle_id = ? ORDER BY id DESC LIMIT 1`, vehicleID).Scan(&stateSince); err == nil {
		out.StateSince = stateSince
	}

	if ok, level, rangeKm, idealRangeKm, _, err := s.store.LatestBatteryReading(vehicleID); err != nil {
		slog.Warn("portal: api/status battery reading failed", "error", err)
	} else if ok {
		out.BatteryLevel = &level
		out.RatedRangeKm = &rangeKm
		out.IdealRangeKm = &idealRangeKm
	}

	if lt, err := s.store.Lifetime(vehicleID); err == nil && (lt.TotalDrives > 0 || lt.OdometerKm > 0) {
		out.OdometerKm = &lt.OdometerKm
	}

	if id, err := s.store.OpenDriveID(vehicleID); err == nil && id != 0 {
		out.ActiveDriveID = &id
	}
	if id, err := s.store.OpenChargingSessionID(vehicleID); err == nil && id != 0 {
		out.ActiveChargeID = &id
	}

	writeJSON(w, out)
}

// handleAPIMeta reports the live database's freshness (its own mtime,
// plus its WAL sidecar's if present, since a write under WAL mode can
// land there without touching the main file) so a frontend can decide
// whether it's worth re-downloading the whole /download snapshot
// rather than doing so unconditionally on a timer.
func (s *Server) handleAPIMeta(w http.ResponseWriter, r *http.Request) {
	type meta struct {
		LastUpdated string `json:"last_updated"`
		SizeBytes   int64  `json:"size_bytes"`
		// Change counters, so a client can tell whether re-downloading
		// the whole database would actually get it anything new.
		//
		// This matters more than it looks. /download takes a fresh
		// consistent snapshot of the entire file on every request -
		// measured at ~1s and ~10MB on a Pi Zero 2 W. Polling it once a
		// minute would be ~14GB/day of transfer AND the same again in
		// SD-card writes, to observe data that changes a few times a
		// day. Polling this endpoint instead is ~50ms and ~100 bytes,
		// and touches no disk.
		//
		// Drives/Charges count only CLOSED rows, so they tick exactly
		// when a drive or charge finishes - the moment new history
		// becomes available. LatestPositionID is a cheap liveness
		// signal that moves during an active drive.
		Drives           int   `json:"drives"`
		Charges          int   `json:"charges"`
		LatestPositionID int64 `json:"latest_position_id"`
	}

	info, err := os.Stat(s.dbPath)
	if err != nil {
		slog.Warn("portal: api/meta stat failed", "error", err)
		http.Error(w, "failed to stat database", http.StatusInternalServerError)
		return
	}
	newest := info.ModTime()
	size := info.Size()
	if walInfo, err := os.Stat(s.dbPath + "-wal"); err == nil {
		if walInfo.ModTime().After(newest) {
			newest = walInfo.ModTime()
		}
	}

	out := meta{LastUpdated: newest.UTC().Format(time.RFC3339Nano), SizeBytes: size}
	// Both counts hit the (vehicle_id, status) index; the position id is
	// an O(1) index lookup rather than a scan. Failures here are
	// non-fatal - the freshness fields above are still worth returning.
	if err := s.store.DB().QueryRow(`SELECT COUNT(*) FROM drives WHERE status = 'closed'`).Scan(&out.Drives); err != nil {
		slog.Warn("portal: api/meta drive count failed", "error", err)
	}
	if err := s.store.DB().QueryRow(`SELECT COUNT(*) FROM charging_sessions WHERE status = 'closed'`).Scan(&out.Charges); err != nil {
		slog.Warn("portal: api/meta charge count failed", "error", err)
	}
	var maxPos sql.NullInt64
	if err := s.store.DB().QueryRow(`SELECT MAX(id) FROM positions`).Scan(&maxPos); err != nil {
		slog.Warn("portal: api/meta position id failed", "error", err)
	}
	out.LatestPositionID = maxPos.Int64

	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("portal: write JSON response failed", "error", err)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data := indexData{VehicleName: "Vehicle", DistanceUnit: s.distanceUnit(), Version: s.version, HasViewer: webui.Available()}

	var vehicleID int64
	row := s.store.DB().QueryRow(`
		SELECT id, COALESCE(display_name, ''), COALESCE(firmware_version, ''),
		       COALESCE(model, ''), COALESCE(marketing_name, '')
		FROM vehicles ORDER BY id LIMIT 1`)
	var displayName, model, marketingName string
	if err := row.Scan(&vehicleID, &displayName, &data.Firmware, &model, &marketingName); err != nil {
		if err != sql.ErrNoRows {
			slog.Warn("portal: query vehicle failed", "error", err)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		indexTemplate.Execute(w, data)
		return
	}
	data.VehicleName = vehicleDisplayName(displayName, model, marketingName)

	if lt, err := s.store.Lifetime(vehicleID); err != nil {
		slog.Warn("portal: lifetime stats failed", "error", err)
	} else {
		data.HasLifetime = lt.TotalDrives > 0 || lt.OdometerKm > 0
		data.OdometerKm = s.toDisplayDistance(lt.OdometerKm)
		data.TotalDrives = lt.TotalDrives
		data.TotalKm = s.toDisplayDistance(lt.TotalKm)
		data.TotalCharges = lt.TotalCharges
		data.TotalKwh = lt.TotalKwh
	}

	today := time.Now().UTC().Format("2006-01-02")
	var todayKm float64
	_ = s.store.DB().QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(distance_km), 0)
		FROM drives WHERE vehicle_id = ? AND status = 'closed' AND date(start_time) = ?
	`, vehicleID, today).Scan(&data.TodayDrives, &todayKm)
	data.TodayKm = s.toDisplayDistance(todayKm)

	if charges, err := s.store.ListCharges(vehicleID, 0); err != nil {
		slog.Warn("portal: list charges failed", "error", err)
	} else if len(charges) > 0 {
		last := charges[0]
		data.HasLastCharge = true
		data.LastChargeFrom, data.LastChargeTo = last.StartBattery, last.EndBattery
		data.LastChargeKwh = last.EnergyAddedKwh
		data.LastChargeEnd = last.EndTime
		data.LastChargeLoc = last.Location

		max := 5
		if len(charges) < max {
			max = len(charges)
		}
		for _, c := range charges[:max] {
			data.RecentCharges = append(data.RecentCharges, recentCharge{
				StartTime: c.StartTime, Location: c.Location, StartBattery: c.StartBattery, EndBattery: c.EndBattery,
				EnergyAddedKwh: c.EnergyAddedKwh, ChargeType: c.ChargeType(), MaxPowerKw: c.MaxChargerPowerKw, Cost: c.Cost,
			})
		}
	}

	if drives, err := s.store.ListDrives(vehicleID, 0); err != nil {
		slog.Warn("portal: list drives failed", "error", err)
	} else {
		max := 5
		if len(drives) < max {
			max = len(drives)
		}
		for _, d := range drives[:max] {
			data.RecentDrives = append(data.RecentDrives, recentDrive{
				StartTime: d.StartTime, FromLoc: d.StartLocation, ToLoc: d.EndLocation,
				DistanceKm: s.toDisplayDistance(d.DistanceKm), DurationMin: d.DurationMin,
				StartBattery: d.StartBattery, EndBattery: d.EndBattery, EfficiencyRatio: d.EfficiencyRatio(),
			})
		}
	}

	if ok, level, rangeKm, idealRangeKm, at, err := s.store.LatestBatteryReading(vehicleID); err != nil {
		slog.Warn("portal: latest battery reading failed", "error", err)
	} else if ok {
		data.HasBattery = true
		data.BatteryLevel = level
		data.RatedRangeKm = s.toDisplayDistance(rangeKm)
		data.IdealRangeKm = s.toDisplayDistance(idealRangeKm)
		data.BatteryAt = at
	}

	var currentState, stateSince string
	if err := s.store.DB().QueryRow(`
		SELECT state, started_at FROM states WHERE vehicle_id = ? ORDER BY id DESC LIMIT 1
	`, vehicleID).Scan(&currentState, &stateSince); err == nil {
		data.HasState = true
		data.CurrentState = currentState
		data.StateSince = stateSince
	}

	if data.HasState {
		if sleep, err := s.store.SleepStats24h(vehicleID, time.Now().UTC()); err != nil {
			slog.Warn("portal: sleep stats failed", "error", err)
		} else {
			data.HasSleepStats = true
			data.AsleepPct24h = sleep.AsleepPct()
		}
	}

	if s.logs != nil {
		data.LogLines = s.logs.Lines()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTemplate.Execute(w, data); err != nil {
		slog.Warn("portal: render index failed", "error", err)
	}
}

// snapshotCache serves /download from a pre-built, VACUUM INTO'd copy of
// the live database instead of building one inside every request.
//
// Why this exists: /download used to snapshot the whole database on
// demand, which put seconds of work in front of the first byte, and the
// old online-backup mechanism restarted from scratch on every concurrent
// write - so while a car was charging (a write every few seconds) a
// download could stall for minutes, and each client that gave up and
// retried kicked off another competing build. The Pi's journal showed a
// loop of "snapshot for download failed ... context canceled".
//
// The cache fixes both halves. Builds are single-flighted, so N concurrent
// requests (a reload, several viewer tabs) share ONE build rather than
// racing. A build runs on a background context, so a client disconnecting
// mid-download can no longer cancel the shared build out from under
// everyone else. And a ready snapshot whose signature still matches the
// live database is served immediately with no build at all.
//
// The signature is the count of closed drives and closed charges, which
// tick only when a trip or a charge FINISHES - the moment genuinely new
// history exists. Deliberately NOT the latest position id: that advances
// every few seconds throughout a drive, and keying on it would have the
// background refresher VACUUM the whole ~67 MB database once a minute for
// the entire drive, even with no viewer open - exactly the kind of
// needless SD-card wear and CPU load this Pi build works to avoid. The
// cost of leaving it out is that a drive or charge still in progress is
// absent from the snapshot until it closes; for a history export and the
// dashboards that read it, that is the right trade. A parked or sleeping
// car changes neither counter and triggers no work at all.
type snapshotCache struct {
	store   *storage.Store
	srcPath string
	dir     string

	mu          sync.Mutex
	readyPath   string        // newest built snapshot (.db), "" until the first build
	readyGzPath string        // gzip of readyPath, served when the client accepts gzip
	readySig    string        // source signature readyPath was built from
	readyAt     time.Time     // when readyPath was built (ServeContent modtime)
	building    chan struct{} // non-nil while a build is in flight; closed when it ends
	buildErr    error         // result of the most recent build
}

// snapshotFile is a ready snapshot: the plain .db, its gzip (may be "" if
// compression failed), and the build time used as the HTTP modtime.
type snapshotFile struct {
	path    string
	gzPath  string
	modTime time.Time
}

func newSnapshotCache(store *storage.Store, srcPath string) *snapshotCache {
	dir := filepath.Join(os.TempDir(), "teslalog-snapshots")
	// A crash or restart can leave a previous run's snapshot behind;
	// clear the directory so it never accumulates 66 MB files.
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("portal: could not create snapshot dir, falling back to temp", "error", err)
		dir = os.TempDir()
	}
	return &snapshotCache{store: store, srcPath: srcPath, dir: dir}
}

// signature is a cheap fingerprint of the live database's history. It
// mirrors the counters /api/meta reports (see handleAPIMeta) so a snapshot
// is considered fresh under exactly the same conditions the viewer uses to
// decide whether to re-download. On any query error it returns a unique
// value, which forces a rebuild rather than serving a possibly-stale file.
func (c *snapshotCache) signature() string {
	var drives, charges int
	db := c.store.DB()
	if err := db.QueryRow(`SELECT COUNT(*) FROM drives WHERE status = 'closed'`).Scan(&drives); err != nil {
		return fmt.Sprintf("err-%d", time.Now().UnixNano())
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM charging_sessions WHERE status = 'closed'`).Scan(&charges); err != nil {
		return fmt.Sprintf("err-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%d-%d", drives, charges)
}

// get returns the path to a snapshot that matches the current source
// signature, building one if the cache is empty or stale. Concurrent
// callers that arrive while a build is running wait for it and share its
// result. ctx cancellation only stops waiting - never the build itself.
func (c *snapshotCache) get(ctx context.Context) (snapshotFile, error) {
	sig := c.signature()

	c.mu.Lock()
	if c.readyPath != "" && c.readySig == sig {
		f := snapshotFile{path: c.readyPath, gzPath: c.readyGzPath, modTime: c.readyAt}
		c.mu.Unlock()
		return f, nil
	}
	if c.building != nil {
		wait := c.building
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return snapshotFile{}, ctx.Err()
		}
		c.mu.Lock()
		f := snapshotFile{path: c.readyPath, gzPath: c.readyGzPath, modTime: c.readyAt}
		err := c.buildErr
		c.mu.Unlock()
		if f.path == "" {
			return snapshotFile{}, err
		}
		return f, nil
	}
	done := make(chan struct{})
	c.building = done
	c.mu.Unlock()

	stamp := time.Now().UnixNano()
	tmp := filepath.Join(c.dir, fmt.Sprintf("snap-%d.db", stamp))
	// context.Background(), not the caller's ctx: a build started for one
	// request is shared by every concurrent and subsequent caller, so a
	// single client hanging up must not abort it. This is the specific
	// regression that produced the "context canceled" build loop.
	buildErr := backup.Snapshot(context.Background(), c.srcPath, tmp)

	// Pre-compress so /download can be served with Content-Encoding: gzip -
	// a SQLite file shrinks ~3x, which is the dominant cost of connecting
	// the viewer over the Pi's wifi. Done here, off the request path, so it
	// costs the Pi nothing per download. A gzip failure is non-fatal: the
	// plain .db is still served, just uncompressed.
	gzTmp := ""
	if buildErr == nil {
		gzTmp = tmp + ".gz"
		if err := backup.GzipFileLevel(tmp, gzTmp, gzip.BestSpeed); err != nil {
			slog.Warn("portal: could not gzip snapshot; serving uncompressed", "error", err)
			_ = os.Remove(gzTmp)
			gzTmp = ""
		}
	}

	c.mu.Lock()
	oldDB, oldGz := c.readyPath, c.readyGzPath
	var f snapshotFile
	if buildErr == nil {
		c.readyPath, c.readyGzPath, c.readySig, c.readyAt = tmp, gzTmp, sig, time.Now()
		f = snapshotFile{path: c.readyPath, gzPath: c.readyGzPath, modTime: c.readyAt}
	} else {
		_ = os.Remove(tmp)
	}
	c.buildErr = buildErr
	c.building = nil
	close(done)
	c.mu.Unlock()

	if buildErr == nil {
		if oldDB != "" && oldDB != tmp {
			_ = os.Remove(oldDB)
		}
		if oldGz != "" && oldGz != gzTmp {
			_ = os.Remove(oldGz)
		}
	}
	return f, buildErr
}

// refresh keeps the cache warm so /download stays instant. It runs get on
// a timer and discards the result: get only rebuilds when the signature
// changed, so an idle car costs one signature() call per tick and no I/O,
// while a just-finished drive is snapshotted within one tick - before the
// viewer's next minute poll asks for it.
func (c *snapshotCache) refresh(ctx context.Context, every time.Duration) {
	warm := func() {
		if _, err := c.get(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("portal: background snapshot refresh failed", "error", err)
		}
	}
	warm() // build once at startup so the first /download is instant too
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			warm()
		}
	}
}

// handleDownload serves the cached snapshot (see snapshotCache) - a
// consistent, VACUUM INTO'd, DELETE-mode copy of the live database - so it
// opens directly in Grafana's SQLite datasource or any other tool with no
// unzip and, crucially, no per-request build latency.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshots.get(r.Context())
	if err != nil {
		if r.Context().Err() != nil {
			return // client hung up; not our error to report
		}
		slog.Error("portal: snapshot for download failed", "error", err)
		http.Error(w, "failed to prepare database snapshot", http.StatusInternalServerError)
		return
	}

	filename := fmt.Sprintf("tesla-%s.db", time.Now().UTC().Format("2006-01-02"))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	// Serve the pre-compressed copy when the client accepts gzip (every
	// browser and download manager does), so the ~67 MB file crosses the
	// wifi at roughly a third the size. Content-Encoding: gzip is transport
	// compression: fetch() in the viewer and a browser's own Save both hand
	// back the decoded .db, so nothing downstream sees a .gz. Range requests
	// are declined for the compressed stream (a range over gzip bytes is
	// meaningless to the client); the viewer fetches the whole file anyway.
	if snap.gzPath != "" && acceptsGzip(r) {
		gz, err := os.Open(snap.gzPath)
		if err == nil {
			defer gz.Close()
			if info, err := gz.Stat(); err == nil {
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
				if _, err := io.Copy(w, gz); err != nil {
					slog.Warn("portal: sending gzip snapshot failed", "error", err)
				}
				return
			}
		}
		slog.Warn("portal: gzip snapshot unavailable, serving uncompressed", "error", err)
	}

	f, err := os.Open(snap.path)
	if err != nil {
		slog.Error("portal: open snapshot for download failed", "error", err)
		http.Error(w, "failed to read database snapshot", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, filename, snap.modTime, f)
}

// acceptsGzip reports whether the client's Accept-Encoding lists gzip.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]), "gzip") {
			return true
		}
	}
	return false
}

// vehicleDisplayName picks the best available name for the car. Tesla
// returns an empty display_name for a vehicle the owner never named in
// the app, which is the common case - so falling straight through to
// the literal word "Vehicle" mislabels every unnamed car. The model is
// known from vehicle_config either way, so "Model Y" is both available
// and what the owner would call it.
func vehicleDisplayName(displayName, model, marketingName string) string {
	if displayName != "" {
		return displayName
	}
	if model == "" {
		return "Vehicle"
	}
	name := "Model " + model
	if marketingName != "" {
		return name + " " + marketingName
	}
	return name
}
