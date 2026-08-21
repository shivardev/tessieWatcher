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
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"teslalog/internal/backup"
	"teslalog/internal/storage"
)

// Server serves the portal's routes: "/" (status + download button),
// "/download" (a fresh database snapshot), "/api/status" (cheap live
// status JSON), and "/api/meta" (cheap freshness-check JSON).
type Server struct {
	store  *storage.Store
	dbPath string
	logs   *LogBuffer
}

// New constructs a Server. store is used read-only, for the status line
// on "/"; dbPath is snapshotted fresh on every "/download" request. logs
// is optional (nil is fine, e.g. in tests) - when provided, its recent
// lines render in a "Recent activity" section on "/".
func New(store *storage.Store, dbPath string, logs *LogBuffer) *Server {
	return &Server{store: store, dbPath: dbPath, logs: logs}
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/download", s.handleDownload)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/meta", s.handleAPIMeta)
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
	VehicleName    string
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
  form { margin: 1.25rem 0; }
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
  <div class="subtitle">teslalog{{if .Firmware}} &middot; firmware {{.Firmware}}{{end}}</div>
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
    <div class="stat"><span class="label">Rated range</span><span class="value">{{printf "%.0f" .RatedRangeKm}} km{{if .IdealRangeKm}} <span style="color:var(--text-dim)">({{printf "%.0f" .IdealRangeKm}} km ideal)</span>{{end}}</span></div>
    {{end}}
    <div class="stat"><span class="label">Drives today</span><span class="value">{{.TodayDrives}}</span></div>
    <div class="stat"><span class="label">Distance today</span><span class="value">{{printf "%.1f" .TodayKm}} km</span></div>
    {{if .HasLastCharge}}
    <div class="stat">
      <span class="label">Last charge</span>
      <span class="value">{{.LastChargeFrom}}% &rarr; {{.LastChargeTo}}% &middot; {{printf "%.1f" .LastChargeKwh}} kWh{{if .LastChargeLoc}} &middot; {{.LastChargeLoc}}{{end}}</span>
    </div>
    {{end}}
  </div>

  <form action="/download" method="get">
    <button type="submit">⬇ Download database (tesla.db)</button>
  </form>

  {{if .HasLifetime}}
  <div class="card">
    <div class="stat"><span class="label">Lifetime odometer</span><span class="value">{{printf "%.0f" .OdometerKm}} km</span></div>
    <div class="stat"><span class="label">Lifetime drives</span><span class="value">{{.TotalDrives}} &middot; {{printf "%.0f" .TotalKm}} km</span></div>
    <div class="stat"><span class="label">Lifetime charging</span><span class="value">{{.TotalCharges}} &middot; {{printf "%.0f" .TotalKwh}} kWh</span></div>
  </div>
  {{end}}

  {{if .RecentDrives}}
  <h2>Recent drives</h2>
  <div class="card" style="padding:0.5rem 1rem; overflow-x:auto;">
    <table>
      <tr><th>When</th><th>From</th><th>To</th><th class="num">km</th><th class="num">min</th><th class="num">%</th><th class="num">eff.</th></tr>
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
	UpdatedAt      string   `json:"updated_at"`
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	out := apiStatus{VehicleName: "Vehicle", UpdatedAt: time.Now().UTC().Format(time.RFC3339)}

	var vehicleID int64
	var displayName, firmware string
	row := s.store.DB().QueryRow(`SELECT id, COALESCE(display_name, ''), COALESCE(firmware_version, '') FROM vehicles ORDER BY id LIMIT 1`)
	if err := row.Scan(&vehicleID, &displayName, &firmware); err != nil {
		if err != sql.ErrNoRows {
			slog.Warn("portal: api/status query vehicle failed", "error", err)
		}
		writeJSON(w, out)
		return
	}
	if displayName != "" {
		out.VehicleName = displayName
	}
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

	writeJSON(w, meta{LastUpdated: newest.UTC().Format(time.RFC3339Nano), SizeBytes: size})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("portal: write JSON response failed", "error", err)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data := indexData{VehicleName: "Vehicle"}

	var vehicleID int64
	row := s.store.DB().QueryRow(`SELECT id, COALESCE(display_name, ''), COALESCE(firmware_version, '') FROM vehicles ORDER BY id LIMIT 1`)
	var displayName string
	if err := row.Scan(&vehicleID, &displayName, &data.Firmware); err != nil {
		if err != sql.ErrNoRows {
			slog.Warn("portal: query vehicle failed", "error", err)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		indexTemplate.Execute(w, data)
		return
	}
	if displayName != "" {
		data.VehicleName = displayName
	}

	if lt, err := s.store.Lifetime(vehicleID); err != nil {
		slog.Warn("portal: lifetime stats failed", "error", err)
	} else {
		data.HasLifetime = lt.TotalDrives > 0 || lt.OdometerKm > 0
		data.OdometerKm = lt.OdometerKm
		data.TotalDrives = lt.TotalDrives
		data.TotalKm = lt.TotalKm
		data.TotalCharges = lt.TotalCharges
		data.TotalKwh = lt.TotalKwh
	}

	today := time.Now().UTC().Format("2006-01-02")
	_ = s.store.DB().QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(distance_km), 0)
		FROM drives WHERE vehicle_id = ? AND status = 'closed' AND date(start_time) = ?
	`, vehicleID, today).Scan(&data.TodayDrives, &data.TodayKm)

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
				DistanceKm: d.DistanceKm, DurationMin: d.DurationMin,
				StartBattery: d.StartBattery, EndBattery: d.EndBattery, EfficiencyRatio: d.EfficiencyRatio(),
			})
		}
	}

	if ok, level, rangeKm, idealRangeKm, at, err := s.store.LatestBatteryReading(vehicleID); err != nil {
		slog.Warn("portal: latest battery reading failed", "error", err)
	} else if ok {
		data.HasBattery = true
		data.BatteryLevel = level
		data.RatedRangeKm = rangeKm
		data.IdealRangeKm = idealRangeKm
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

// handleDownload takes a fresh, consistent online-backup snapshot of the
// live database (safe under WAL, via internal/backup.Snapshot) and
// serves it uncompressed, so it can be opened directly by Grafana's
// SQLite datasource plugin or any other tool without an extra unzip
// step. The snapshot is written to a temp file and removed once served.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	tmpPath := filepath.Join(os.TempDir(), fmt.Sprintf(".teslalog-portal-snapshot-%d.db", time.Now().UnixNano()))
	defer os.Remove(tmpPath)

	if err := backup.Snapshot(r.Context(), s.dbPath, tmpPath); err != nil {
		slog.Error("portal: snapshot for download failed", "error", err)
		http.Error(w, "failed to prepare database snapshot", http.StatusInternalServerError)
		return
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		slog.Error("portal: open snapshot for download failed", "error", err)
		http.Error(w, "failed to read database snapshot", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	filename := fmt.Sprintf("tesla-%s.db", time.Now().UTC().Format("2006-01-02"))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	http.ServeContent(w, r, filename, time.Now(), f)
}
