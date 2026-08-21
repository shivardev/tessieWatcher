// Command teslalog is a single-binary TeslaMate-style vehicle logger:
// Owner API + Tesla streaming + sleep-aware polling + SQLite storage.
// See README.md for setup and internal/*/doc comments for design notes.
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"teslalog/internal/backup"
	"teslalog/internal/config"
	"teslalog/internal/runner"
	"teslalog/internal/storage"
	"teslalog/internal/tesla"
)

const version = "0.2.1"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var configPath string
	cmd := os.Args[1]
	args := os.Args[2:]

	// Every subcommand accepts -config, defaulting to /etc/teslalog/config.toml.
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.StringVar(&configPath, "config", defaultConfigPath(), "path to config.toml")

	var err error
	switch cmd {
	case "auth":
		fs.Parse(args)
		err = runAuth(configPath)
	case "run":
		fs.Parse(args)
		err = runDaemon(configPath)
	case "wake":
		fs.Parse(args)
		err = runWake(configPath)
	case "status":
		fs.Parse(args)
		err = runStatus(configPath)
	case "backup":
		fs.Parse(args)
		err = runBackup(configPath)
	case "export":
		err = runExport(configPath, args)
	case "auth-callback":
		// Not for interactive use - see runAuthCallback. No -config flag:
		// os.Args[2] is the callback URL, which could itself start with a
		// "-" in principle, so this case deliberately skips fs.Parse(args).
		err = runAuthCallback(args)
	case "update":
		err = runUpdate()
	case "version", "-v", "--version":
		fmt.Printf("teslalog %s\n", version)
		return
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `teslalog - lightweight TeslaMate-style vehicle logger (Owner API + streaming + SQLite)

Usage:
  teslalog auth [-config path]              interactive Tesla account login
  teslalog run [-config path]                run the logging daemon (foreground; use systemd for real deployment)
  teslalog status [-config path]             print today's drives/energy and last charge
  teslalog wake [-config path]               explicitly wake the vehicle (never done automatically)
  teslalog backup [-config path]             run one SQLite backup immediately
  teslalog export drives [-year N] [-out f]  export closed drives to CSV
  teslalog export charges [-year N] [-out f] export closed charging sessions to CSV
  teslalog update                            self-update to the latest GitHub release
  teslalog version                           print version

Config file: TOML, defaults to /etc/teslalog/config.toml (see config.example.toml).
`)
}

func defaultConfigPath() string {
	if p := os.Getenv("TESLALOG_CONFIG"); p != "" {
		return p
	}
	return "/etc/teslalog/config.toml"
}

func loadConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return cfg, err
	}
	for _, dir := range []string{filepath.Dir(cfg.Database), filepath.Dir(cfg.TokenFile)} {
		if dir != "" && dir != "." {
			_ = os.MkdirAll(dir, 0o755)
		}
	}
	return cfg, nil
}

// ---- auth ----

func runAuth(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	p, err := tesla.NewPKCE()
	if err != nil {
		return fmt.Errorf("generate PKCE parameters: %w", err)
	}

	relayPath := authCallbackRelayPath()
	_ = os.Remove(relayPath) // clear any stale callback left over from a previous attempt

	authURL := tesla.AuthorizeURL(cfg.API, p)
	fmt.Println("1. Open this URL in a browser and log in to your Tesla account:")
	fmt.Println()
	fmt.Println("   " + authURL)
	fmt.Println()
	fmt.Println("2. After login, Tesla redirects to a tesla://auth/callback?code=...&state=...")
	fmt.Println("   URL - the same one the official Tesla app registers itself to handle. Most")
	fmt.Println("   browsers either fail silently (the page just hangs on \"Loading...\" forever,")
	fmt.Println("   with nothing to copy) or show an \"Open Tesla?\"/\"can't open this link\" prompt.")
	fmt.Println("   That's expected - see README's Authentication section for why.")
	fmt.Println("3a. If you've already run deploy/register-tesla-protocol.ps1 once on THIS")
	fmt.Println("    machine, this step captures the redirect automatically - just wait, nothing")
	fmt.Println("    more to do here.")
	fmt.Println("3b. Otherwise, get the full tesla://auth/callback?code=...&state=... URL however")
	fmt.Println("    your browser exposes it (an error dialog's text, or DevTools' Network/Console")
	fmt.Println("    tab if you opened it before clicking login) and paste it (or just the 'code'")
	fmt.Println("    value) below.")
	fmt.Println()
	fmt.Print("Paste the redirect URL or code here (or leave this - auto-capture will fill it in): ")

	line, err := waitForCallback(relayPath)
	if err != nil {
		return fmt.Errorf("read callback: %w", err)
	}

	code, err := tesla.ParseCallback(line, p)
	if err != nil {
		return err
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	tokens, err := tesla.ExchangeCode(httpClient, cfg.API, p, code)
	if err != nil {
		return fmt.Errorf("exchange code for tokens: %w", err)
	}

	if err := tesla.SaveTokenFile(cfg.TokenFile, tokens); err != nil {
		return err
	}

	fmt.Printf("\nSuccess. Tokens saved to %s (mode 0600).\n", cfg.TokenFile)
	fmt.Println("Run `teslalog run` to start logging, or `teslalog status` once you have data.")
	return nil
}

// authCallbackRelayPath is the fixed local file `teslalog auth` polls for
// a captured tesla://auth/callback URL, and that a separately-invoked
// `teslalog auth-callback <url>` process (see runAuthCallback) writes it
// to. They're different OS processes - the tesla:// protocol handler
// Windows launches on redirect is a brand-new process, not a message
// delivered to the already-running `teslalog auth` - so a shared file is
// the simplest way to hand the URL from one to the other.
func authCallbackRelayPath() string {
	return filepath.Join(os.TempDir(), "teslalog-auth-callback.txt")
}

// waitForCallback blocks until the login redirect is available by
// whichever path gets there first: a human pasting it into stdin (the
// original, universal path - works from any device, including one that
// isn't running teslalog at all), or a registered tesla:// protocol
// handler on this machine writing it to authCallbackRelayPath (see
// runAuthCallback and deploy/register-tesla-protocol.ps1). Without that
// registration, only the stdin path ever fires, so this is a strict
// superset of the old "just read a line from stdin" behavior.
func waitForCallback(relayPath string) (string, error) {
	result := make(chan string, 1)
	errCh := make(chan error, 1)

	go func() {
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil {
			errCh <- fmt.Errorf("read input: %w", err)
			return
		}
		result <- line
	}()

	go func() {
		for {
			data, err := os.ReadFile(relayPath)
			if err == nil && len(data) > 0 {
				result <- string(data)
				return
			}
			time.Sleep(400 * time.Millisecond)
		}
	}()

	select {
	case line := <-result:
		return line, nil
	case err := <-errCh:
		return "", err
	}
}

// runAuthCallback is invoked by the OS-registered tesla:// URL protocol
// handler (deploy/register-tesla-protocol.ps1), not by a user directly:
// Windows launches it as a brand-new process, passing the full redirect
// URL as its one argument. It just relays that URL to a concurrently
// running `teslalog auth` process via authCallbackRelayPath, since
// there's no other channel between these two separate processes.
func runAuthCallback(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: teslalog auth-callback <tesla://auth/callback?code=...&state=...>")
	}
	if err := os.WriteFile(authCallbackRelayPath(), []byte(args[0]), 0o600); err != nil {
		return fmt.Errorf("relay callback: %w", err)
	}
	fmt.Println("Login captured - return to the `teslalog auth` window, it should complete automatically.")
	return nil
}

// ---- run ----

func runDaemon(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("teslalog starting", "version", version, "database", cfg.Database)
	err = runner.Run(ctx, cfg)
	if err != nil && ctx.Err() != nil {
		// Clean shutdown via signal, not a real error.
		slog.Info("teslalog stopped")
		return nil
	}
	return err
}

// ---- wake ----

func runWake(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	client, err := tesla.NewClient(cfg.API, tesla.FileTokenStore{Path: cfg.TokenFile})
	if err != nil {
		return err
	}
	ctx := context.Background()
	vehicles, err := client.ListVehicles(ctx)
	if err != nil {
		return err
	}
	if len(vehicles) == 0 {
		return fmt.Errorf("no vehicles on this account")
	}
	target := vehicles[0]
	for _, v := range vehicles {
		if cfg.VIN != "" && v.VIN == cfg.VIN {
			target = v
		}
	}
	fmt.Printf("Waking %s (%s)... this is a manual, explicit action.\n", target.DisplayName, target.VIN)
	// wake_up (like vehicle_data) takes VehicleSummary.ID, not VehicleID -
	// see VehicleSummary's doc comment in internal/tesla/ownerapi.go.
	return client.WakeUp(ctx, target.ID)
}

// ---- status ----

func runStatus(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer store.Close()

	var vehicleID int64
	row := store.DB().QueryRow(`SELECT id FROM vehicles ORDER BY id LIMIT 1`)
	if err := row.Scan(&vehicleID); err != nil {
		fmt.Println("No vehicle recorded yet - has `teslalog run` completed at least one poll?")
		return nil
	}

	today := time.Now().UTC().Format("2006-01-02")
	rows, err := store.DB().Query(`
		SELECT start_time, end_time, distance_km, start_battery_level, end_battery_level
		FROM drives WHERE vehicle_id = ? AND status = 'closed' AND date(start_time) = ?
		ORDER BY start_time
	`, vehicleID, today)
	if err != nil {
		return err
	}
	defer rows.Close()

	var driveCount int
	var totalDistance float64
	for rows.Next() {
		var start, end string
		var distance float64
		var startBat, endBat int
		if err := rows.Scan(&start, &end, &distance, &startBat, &endBat); err != nil {
			return err
		}
		driveCount++
		totalDistance += distance
	}

	fmt.Println("Today")
	fmt.Printf("  Drives:   %d\n", driveCount)
	fmt.Printf("  Distance: %.1f km\n", totalDistance)

	var lastStart, lastEnd string
	var lastStartBat, lastEndBat int
	var lastEnergy float64
	err = store.DB().QueryRow(`
		SELECT start_time, end_time, start_battery_level, end_battery_level, charge_energy_added_kwh
		FROM charging_sessions WHERE vehicle_id = ? AND status = 'closed'
		ORDER BY start_time DESC LIMIT 1
	`, vehicleID).Scan(&lastStart, &lastEnd, &lastStartBat, &lastEndBat, &lastEnergy)
	if err == nil {
		fmt.Println()
		fmt.Println("Last charge")
		fmt.Printf("  %d%% -> %d%%\n", lastStartBat, lastEndBat)
		fmt.Printf("  %.1f kWh\n", lastEnergy)
		fmt.Printf("  ended %s\n", lastEnd)
	}
	return nil
}

// ---- backup ----

func runBackup(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	path, err := backup.Run(context.Background(), cfg.Database, cfg.Backup.Dir, cfg.Backup.RetentionDays, time.Now())
	if err != nil {
		return err
	}
	fmt.Println("backup written:", path)
	return nil
}

// ---- export ----

func runExport(configPath string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: teslalog export <drives|charges> [-year N] [-out file.csv]")
	}
	kind := args[0]
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	var year int
	var out string
	var cfgPath string
	fs.IntVar(&year, "year", 0, "filter by year (0 = all)")
	fs.StringVar(&out, "out", "", "output file (default: stdout)")
	fs.StringVar(&cfgPath, "config", configPath, "path to config.toml")
	fs.Parse(args[1:])

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer store.Close()

	var vehicleID int64
	if err := store.DB().QueryRow(`SELECT id FROM vehicles ORDER BY id LIMIT 1`).Scan(&vehicleID); err != nil {
		return fmt.Errorf("no vehicle recorded yet")
	}

	w := os.Stdout
	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	cw := csv.NewWriter(w)
	defer cw.Flush()

	switch kind {
	case "drives":
		drives, err := store.ListDrives(vehicleID, year)
		if err != nil {
			return err
		}
		cw.Write([]string{
			"id", "start_time", "end_time", "start_location", "end_location", "distance_km", "duration_min",
			"start_battery_pct", "end_battery_pct", "max_speed_kmh", "rated_range_lost_km", "efficiency_ratio",
		})
		for _, d := range drives {
			cw.Write([]string{
				strconv.FormatInt(d.ID, 10), d.StartTime, d.EndTime, d.StartLocation, d.EndLocation,
				fmt.Sprintf("%.2f", d.DistanceKm), fmt.Sprintf("%.1f", d.DurationMin),
				strconv.Itoa(d.StartBattery), strconv.Itoa(d.EndBattery),
				fmt.Sprintf("%.1f", d.MaxSpeedKmh), fmt.Sprintf("%.2f", d.RangeLostKm()), fmt.Sprintf("%.3f", d.EfficiencyRatio()),
			})
		}
	case "charges":
		charges, err := store.ListCharges(vehicleID, year)
		if err != nil {
			return err
		}
		cw.Write([]string{
			"id", "start_time", "end_time", "location", "start_battery_pct", "end_battery_pct",
			"charge_type", "energy_added_kwh", "energy_used_kwh", "max_charger_power_kw", "cost", "kwh_per_rated_km",
		})
		for _, c := range charges {
			cw.Write([]string{
				strconv.FormatInt(c.ID, 10), c.StartTime, c.EndTime, c.Location,
				strconv.Itoa(c.StartBattery), strconv.Itoa(c.EndBattery),
				c.ChargeType(), fmt.Sprintf("%.2f", c.EnergyAddedKwh), fmt.Sprintf("%.2f", c.EnergyUsedKwh),
				fmt.Sprintf("%.2f", c.MaxChargerPowerKw), fmt.Sprintf("%.2f", c.Cost), fmt.Sprintf("%.3f", c.KwhPerRatedKm()),
			})
		}
	default:
		return fmt.Errorf("unknown export kind %q (want drives or charges)", kind)
	}
	return nil
}
