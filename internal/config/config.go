// Package config loads and validates TeslaLog Mini's configuration file.
//
// The config file is TOML (not YAML): YAML's most common Go library
// lives at the gopkg.in vanity import, which this build environment's
// network egress does not allow, whereas github.com/BurntSushi/toml is
// a canonical (non-redirected) GitHub import. Functionally it makes no
// difference to the deployed Pi — TOML is just as easy to hand-edit.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration for teslalog, after duration
// strings have been parsed.
type Config struct {
	Database  string
	TokenFile string
	VIN       string

	Polling   PollingConfig
	Streaming StreamingConfig
	Backup    BackupConfig
	API       APIConfig
	Vehicle   VehicleConfig
	Charging  ChargingConfig
}

// VehicleConfig holds user-supplied (not API-reported) vehicle facts.
type VehicleConfig struct {
	// EfficiencyWhKm is a Wh/km consumption estimate used the same way
	// TeslaMate uses it: purely informational, stored alongside the
	// vehicle row for whatever reads the database to use in its own
	// range projections. teslalog itself does not compute anything
	// from it. Leave 0 to skip.
	EfficiencyWhKm float64
}

// ChargingConfig controls the two charging figures the Owner API does
// not report directly and that TeslaMate estimates: energy actually
// drawn from the wall, and cost.
type ChargingConfig struct {
	// Efficiency (0-1) models charging losses: charge_energy_used_kwh
	// is estimated as charge_energy_added_kwh / Efficiency. Leave 0 to
	// skip recording an estimate at all (the column stays NULL).
	Efficiency float64
	// PricePerKwh, if set, is multiplied by charge_energy_added_kwh to
	// populate charging_sessions.cost. Leave 0 for no cost tracking
	// (matches TeslaMate's behavior with no geofence price configured).
	PricePerKwh float64
}

type PollingConfig struct {
	// DrivingInterval, ChargingInterval and OnlineInterval are how often
	// we call vehicle_data in each of those three "awake" states. These
	// default to TeslaMate's own published defaults (Vehicle.driving_interval/
	// charging_interval/default_interval in lib/teslamate/vehicles/vehicle.ex:
	// 2.5s / 5s / 15s) rather than one flat interval, so drive tracks in
	// particular have TeslaMate-comparable position density.
	//
	// NOT reproduced: TeslaMate additionally (a) backs its REST poll
	// rate off to the slower default_interval even while driving when
	// its streaming connection is actively delivering telemetry (since
	// the stream itself supplies fine-grained position data in that
	// case), (b) adaptively tightens/loosens the charging interval via
	// a formula based on sample count, and (c) applies exponential
	// backoff on repeated fetch errors. teslalog's own streaming client
	// supplements REST-derived positions the same way, but the REST
	// poll cadence itself is a fixed, configurable interval rather than
	// that full adaptive/backoff state machine - simpler and safer to
	// reason about for a single-vehicle logger, at the cost of not
	// being a byte-for-byte scheduler port.
	DrivingInterval  time.Duration
	ChargingInterval time.Duration
	OnlineInterval   time.Duration
	// IdleTimeout is how long the vehicle can sit IDLE (online, parked,
	// not charging) before we stop polling and let it sleep.
	IdleTimeout time.Duration
	// SuspendedCheckInterval is how often we perform a *lightweight*
	// check (no vehicle_data, no wake) while SUSPENDED/ASLEEP to see if
	// the car has become active on its own.
	SuspendedCheckInterval time.Duration
}

type StreamingConfig struct {
	Enabled bool
	URL     string
}

type BackupConfig struct {
	Enabled       bool
	Dir           string
	RetentionDays int
	Interval      time.Duration
}

type APIConfig struct {
	OwnerAPIBaseURL string
	SSOBaseURL      string
	ClientID        string
	UserAgent       string
}

// rawConfig mirrors Config but with durations as human strings ("30s",
// "3m", "15m") since TOML has no native duration type.
type rawConfig struct {
	Database  string `toml:"database"`
	TokenFile string `toml:"token_file"`
	VIN       string `toml:"vin"`

	Polling struct {
		DrivingInterval        string `toml:"driving_interval"`
		ChargingInterval       string `toml:"charging_interval"`
		OnlineInterval         string `toml:"online_interval"`
		IdleTimeout            string `toml:"idle_timeout"`
		SuspendedCheckInterval string `toml:"suspended_check_interval"`
	} `toml:"polling"`

	Streaming struct {
		// *bool (not bool) so we can tell "not set in the file" (nil,
		// keep the default) apart from an explicit "enabled = false"
		// (must actually turn it off) - a plain bool can't distinguish
		// those and would silently ignore an explicit false.
		Enabled *bool  `toml:"enabled"`
		URL     string `toml:"url"`
	} `toml:"streaming"`

	Backup struct {
		Enabled       *bool  `toml:"enabled"`
		Dir           string `toml:"dir"`
		RetentionDays int    `toml:"retention_days"`
		Interval      string `toml:"interval"`
	} `toml:"backup"`

	API struct {
		OwnerAPIBaseURL string `toml:"owner_api_base_url"`
		SSOBaseURL      string `toml:"sso_base_url"`
		ClientID        string `toml:"client_id"`
		UserAgent       string `toml:"user_agent"`
	} `toml:"api"`

	Vehicle struct {
		EfficiencyWhKm float64 `toml:"efficiency_wh_km"`
	} `toml:"vehicle"`

	Charging struct {
		Efficiency  float64 `toml:"efficiency"`
		PricePerKwh float64 `toml:"price_per_kwh"`
	} `toml:"charging"`
}

// Default returns a Config populated with sane defaults matching
// TeslaMate-style behavior (3 minute idle timeout before suspending
// polling, no aggressive wake-ups).
func Default() Config {
	return Config{
		Database:  "/var/lib/teslalog/tesla.db",
		TokenFile: "/var/lib/teslalog/tokens.json",
		Polling: PollingConfig{
			// TeslaMate's own published defaults (Vehicle.driving_interval/
			// charging_interval/default_interval).
			DrivingInterval:        2500 * time.Millisecond,
			ChargingInterval:       5 * time.Second,
			OnlineInterval:         15 * time.Second,
			IdleTimeout:            3 * time.Minute,
			SuspendedCheckInterval: 15 * time.Minute,
		},
		Streaming: StreamingConfig{
			Enabled: true,
			URL:     "wss://streaming.vn.teslamotors.com/streaming/",
		},
		Backup: BackupConfig{
			Enabled:       true,
			Dir:           "/var/lib/teslalog/backups",
			RetentionDays: 30,
			Interval:      24 * time.Hour,
		},
		API: APIConfig{
			OwnerAPIBaseURL: "https://owner-api.teslamotors.com",
			SSOBaseURL:      "https://auth.tesla.com",
			// Same public client_id historically used by the official
			// Tesla mobile app for the SSO PKCE flow.
			ClientID:  "ownerapi",
			UserAgent: "teslalog/0.1",
		},
	}
}

// Load reads a TOML config file at path, applying defaults for any
// fields left unset or blank. If path does not exist, defaults are
// returned as-is (not an error) so `teslalog run` works out of the box.
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}

	var raw rawConfig
	if err := toml.Unmarshal(data, &raw); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}

	if raw.Database != "" {
		cfg.Database = raw.Database
	}
	if raw.TokenFile != "" {
		cfg.TokenFile = raw.TokenFile
	}
	cfg.VIN = raw.VIN

	if d, err := parseDurationOr(raw.Polling.DrivingInterval, cfg.Polling.DrivingInterval); err != nil {
		return cfg, fmt.Errorf("polling.driving_interval: %w", err)
	} else {
		cfg.Polling.DrivingInterval = d
	}
	if d, err := parseDurationOr(raw.Polling.ChargingInterval, cfg.Polling.ChargingInterval); err != nil {
		return cfg, fmt.Errorf("polling.charging_interval: %w", err)
	} else {
		cfg.Polling.ChargingInterval = d
	}
	if d, err := parseDurationOr(raw.Polling.OnlineInterval, cfg.Polling.OnlineInterval); err != nil {
		return cfg, fmt.Errorf("polling.online_interval: %w", err)
	} else {
		cfg.Polling.OnlineInterval = d
	}
	if d, err := parseDurationOr(raw.Polling.IdleTimeout, cfg.Polling.IdleTimeout); err != nil {
		return cfg, fmt.Errorf("polling.idle_timeout: %w", err)
	} else {
		cfg.Polling.IdleTimeout = d
	}
	if d, err := parseDurationOr(raw.Polling.SuspendedCheckInterval, cfg.Polling.SuspendedCheckInterval); err != nil {
		return cfg, fmt.Errorf("polling.suspended_check_interval: %w", err)
	} else {
		cfg.Polling.SuspendedCheckInterval = d
	}

	if raw.Streaming.Enabled != nil {
		cfg.Streaming.Enabled = *raw.Streaming.Enabled
	}
	if raw.Streaming.URL != "" {
		cfg.Streaming.URL = raw.Streaming.URL
	}

	if raw.Backup.Dir != "" {
		cfg.Backup.Dir = raw.Backup.Dir
	}
	if raw.Backup.RetentionDays != 0 {
		cfg.Backup.RetentionDays = raw.Backup.RetentionDays
	}
	if d, err := parseDurationOr(raw.Backup.Interval, cfg.Backup.Interval); err != nil {
		return cfg, fmt.Errorf("backup.interval: %w", err)
	} else {
		cfg.Backup.Interval = d
	}
	if raw.Backup.Enabled != nil {
		cfg.Backup.Enabled = *raw.Backup.Enabled
	}

	if raw.API.OwnerAPIBaseURL != "" {
		cfg.API.OwnerAPIBaseURL = raw.API.OwnerAPIBaseURL
	}
	if raw.API.SSOBaseURL != "" {
		cfg.API.SSOBaseURL = raw.API.SSOBaseURL
	}
	if raw.API.ClientID != "" {
		cfg.API.ClientID = raw.API.ClientID
	}
	if raw.API.UserAgent != "" {
		cfg.API.UserAgent = raw.API.UserAgent
	}

	cfg.Vehicle.EfficiencyWhKm = raw.Vehicle.EfficiencyWhKm
	cfg.Charging.Efficiency = raw.Charging.Efficiency
	cfg.Charging.PricePerKwh = raw.Charging.PricePerKwh

	return cfg, nil
}

func parseDurationOr(s string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	return time.ParseDuration(s)
}
