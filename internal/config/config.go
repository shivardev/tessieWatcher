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
	"strings"
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
	Portal    PortalConfig
	Geocoding GeocodingConfig
	Geofences []GeofenceConfig
	API       APIConfig
	Vehicle   VehicleConfig
	Charging  ChargingConfig
}

// GeofenceConfig is one user-named circular zone (a config.toml
// [[geofence]] entry) - checked before any reverse-geocoding lookup, so
// your most common places (home, work) resolve for free with no network
// call at all. See internal/geocode.
type GeofenceConfig struct {
	Name    string
	Lat     float64
	Lng     float64
	RadiusM float64

	// Per-geofence charging price. Real rates differ by location -
	// home vs a paid apartment charger vs a free one at a store - so
	// one global rate can't produce correct costs for anyone with more
	// than one regular charging spot. Matches TeslaMate's own
	// per-geofence pricing. See geocode.Geofence's equivalent fields.
	//
	// HasPricing is set by Load when the file actually specified a
	// price for this geofence, distinguishing a genuinely free charger
	// (cost_per_unit = 0) from one with no pricing configured at all
	// (cost left unknown).
	HasPricing  bool
	BillingType string
	CostPerUnit float64
	SessionFee  float64
}

// GeocodingConfig controls the optional, off-by-default fallback for
// resolving a drive/charge location that didn't match any configured
// geofence: a reverse-geocoding HTTP lookup against an OSM
// Nominatim-compatible service, cached in the database so the same spot
// is never looked up twice. Off by default since it's a network
// dependency to a third-party service, unlike geofences which are free.
type GeocodingConfig struct {
	Enabled bool
	BaseURL string
	// UserAgent identifies this installation to the geocoding service.
	// Nominatim's usage policy requires a descriptive User-Agent (ideally
	// with contact info) - see
	// https://operations.osmfoundation.org/policies/nominatim/. If you're
	// making more than occasional personal-use requests, that policy asks
	// you to self-host your own Nominatim instance instead and point
	// BaseURL at it.
	UserAgent string
}

// PortalConfig controls the read-only HTTP portal (internal/portal): a
// status page plus a button that downloads a fresh database snapshot.
// On by default - see config.example.toml and the README's Portal
// section before leaving this on for anything other than a trusted home
// LAN, or if this machine is reachable from the public internet - there
// is no login, and the database is a complete log of everywhere the
// vehicle has been and when.
type PortalConfig struct {
	Enabled bool
	// Addr is the address net/http.Server listens on, e.g. ":8083" (all
	// interfaces - reachable from other devices on the LAN) or
	// "127.0.0.1:8083" (this machine only).
	Addr string
	// Units is "metric" (default, matching every distance value stored
	// in the database, which is always km regardless of this setting)
	// or "imperial" - only ever affects how the portal's own HTML page
	// displays numbers; it never changes what's written to storage, so
	// switching it later doesn't reinterpret any existing data.
	Units string
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
	// PricePerKwh is a FALLBACK rate, used only for charges that
	// happen outside every priced geofence (see
	// GeofenceConfig.CostPerUnit, which takes precedence and is how
	// per-location rates - home vs a paid apartment charger vs a free
	// one - are actually expressed). Leave 0 to record no cost for
	// unpriced locations, matching TeslaMate's behavior with no
	// geofence price configured.
	PricePerKwh float64
	// FreeSupercharging: if true, any charge at a Tesla-branded fast
	// charger costs 0 regardless of any other pricing. Matches
	// TeslaMate's own car_settings.free_supercharging (verified
	// against lib/teslamate/log.ex's put_cost, which short-circuits on
	// fast_charger_type starting with "Tesla").
	FreeSupercharging bool
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
	// not charging) before we stop polling at OnlineInterval and switch
	// to the much cheaper SuspendedCheckInterval cadence instead - i.e.
	// how long we keep watching closely before deciding to leave it
	// alone. Matches TeslaMate's own car_settings.suspend_after_idle_min
	// default exactly (15 - verified directly against
	// lib/teslamate/settings/car_settings.ex's schema, not guessed):
	// an earlier default of 3 minutes here meant teslalog gave up on
	// active polling 5x sooner than TeslaMate does, e.g. missing
	// sentry-mode-relevant activity during a 5-10 minute errand stop
	// that TeslaMate would still have been watching closely.
	IdleTimeout time.Duration
	// SuspendedCheckInterval is how often we perform a *lightweight*
	// check (no vehicle_data, no wake) once SUSPENDED (see IdleTimeout)
	// to see if the car has become active on its own. Matches
	// TeslaMate's own car_settings.suspend_min default exactly (21).
	SuspendedCheckInterval time.Duration
	// AsleepInterval is the same kind of lightweight, non-waking check,
	// but for ASLEEP and OFFLINE specifically. Matches TeslaMate's own
	// @asleep_interval exactly (30s - verified directly against its
	// source, lib/teslamate/vehicles/vehicle.ex; applies identically to
	// both states there, confirmed by its `when state in [:asleep,
	// :offline]` guard clause). Found live, head-to-head against a real
	// TeslaMate instance polling the same car: with ASLEEP/OFFLINE
	// bucketed under a single, much longer interval (the original
	// behavior here - see isAsleepLike), teslalog missed the first ~10
	// minutes and ~11 km of a real drive that started from OFFLINE,
	// where TeslaMate caught the same drive within about a minute.
	// ListVehicles is cheap and never wakes the car regardless of how
	// often it's called - TeslaMate's own default reflects that there's
	// no cost to checking ASLEEP/OFFLINE this often, only SUSPENDED
	// (a deliberate choice, see IdleTimeout) is meant to be slow.
	AsleepInterval time.Duration
	// DriveTimeout is how long a drive can go OFFLINE (not ASLEEP -
	// see vehicle.Machine.OnUnreachable) before it's considered
	// abandoned and closed using its last known position. Matches
	// TeslaMate's own @drive_timeout_min exactly (verified directly
	// against its source, lib/teslamate/vehicles/vehicle.ex) -
	// deliberately not invented independently, given how easy it is
	// to get this kind of edge case subtly wrong without checking.
	DriveTimeout time.Duration
	// OfflineChargeMinGap is how long the vehicle must have been
	// unobservable before a range gain across that gap is treated as
	// "it charged somewhere we couldn't see" (see
	// vehicle.Machine.OnBackOnline). Defaults to TeslaMate's own
	// threshold; exists as a config field mainly so tests can drive
	// the full detection path without waiting out five real minutes.
	OfflineChargeMinGap time.Duration
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

	// At is a local wall-clock time ("03:00") to run the daily backup.
	// Preferred over Interval: an interval counts from process start, so
	// every restart moves the backup, and a daemon restarted each
	// afternoon would only ever snapshot the afternoon. A fixed local
	// hour also lands when the car is reliably parked. Empty means fall
	// back to Interval.
	At string

	// Uploads are offsite copies, made after each successful local
	// backup. A backup that only ever exists on the same SD card as the
	// database it came from does not survive the failure it exists for.
	Uploads []UploadConfig

	// RclonePath/RcloneConfig locate the rclone binary and its
	// configuration. rclone is used rather than a native Go client
	// because it already holds working, refreshable credentials for the
	// destinations people actually have - reimplementing Google's OAuth
	// dance in teslalog would mean a fresh consent flow for no gain.
	RclonePath   string
	RcloneConfig string
}

// UploadConfig is one offsite destination for finished backups, named as
// an rclone remote and path ("googledrive:teslalog-backups").
type UploadConfig struct {
	// Name is for log lines only, so a failure says which destination
	// failed without printing the remote path.
	Name   string
	Remote string

	// RetentionDays prunes the destination. Zero keeps everything -
	// the right default for somewhere that is not short of space, and
	// never something to infer.
	RetentionDays int
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
		AsleepInterval         string `toml:"asleep_interval"`
		DriveTimeout           string `toml:"drive_timeout"`
		OfflineChargeMinGap    string `toml:"offline_charge_min_gap"`
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
		At            string `toml:"at"`
		RclonePath    string `toml:"rclone_path"`
		RcloneConfig  string `toml:"rclone_config"`
		Upload        []struct {
			Name          string `toml:"name"`
			Remote        string `toml:"remote"`
			RetentionDays int    `toml:"retention_days"`
		} `toml:"upload"`
	} `toml:"backup"`

	Portal struct {
		Enabled *bool  `toml:"enabled"`
		Addr    string `toml:"addr"`
		Units   string `toml:"units"`
	} `toml:"portal"`

	Geocoding struct {
		Enabled   *bool  `toml:"enabled"`
		BaseURL   string `toml:"base_url"`
		UserAgent string `toml:"user_agent"`
	} `toml:"geocoding"`

	// [[geofence]] array-of-tables - see GeofenceConfig.
	Geofence []struct {
		Name    string  `toml:"name"`
		Lat     float64 `toml:"lat"`
		Lng     float64 `toml:"lng"`
		RadiusM float64 `toml:"radius_m"`
		// *float64 (not float64) so "not priced at all" is
		// distinguishable from an explicit 0 meaning free - see
		// GeofenceConfig.HasPricing.
		CostPerUnit *float64 `toml:"cost_per_unit"`
		BillingType string   `toml:"billing_type"`
		SessionFee  float64  `toml:"session_fee"`
	} `toml:"geofence"`

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
		Efficiency        float64 `toml:"efficiency"`
		PricePerKwh       float64 `toml:"price_per_kwh"`
		FreeSupercharging *bool   `toml:"free_supercharging"`
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
			DrivingInterval:  2500 * time.Millisecond,
			ChargingInterval: 5 * time.Second,
			OnlineInterval:   15 * time.Second,
			// The following four all match TeslaMate's own real
			// defaults exactly, verified directly against its source
			// (car_settings.ex's schema, vehicle.ex's @asleep_interval/
			// @drive_timeout_min) rather than reconstructed from memory
			// or documentation - see each field's doc comment on
			// PollingConfig for why that distinction mattered here.
			IdleTimeout:            15 * time.Minute,
			SuspendedCheckInterval: 21 * time.Minute,
			AsleepInterval:         30 * time.Second,
			DriveTimeout:           15 * time.Minute,
			// Matches vehicle.OfflineChargeMinGap (kept as a literal
			// here rather than importing internal/vehicle, which would
			// make config depend on it - config is meant to be a leaf).
			OfflineChargeMinGap: 5 * time.Minute,
		},
		Streaming: StreamingConfig{
			Enabled: true,
			URL:     "wss://streaming.vn.teslamotors.com/streaming/",
		},
		Backup: BackupConfig{
			Enabled:       true,
			Dir:           "/var/lib/teslalog/backups",
			// Seven days locally: this is the scratch copy on a small SD
			// card, and the copies that matter live offsite.
			RetentionDays: 7,
			Interval:      24 * time.Hour,
			// 03:00 local - the car is reliably parked, and nothing else
			// teslalog does is competing for the Pi at that hour.
			At:           "03:00",
			RclonePath:   "rclone",
			RcloneConfig: "/etc/teslalog/rclone.conf",
		},
		Portal: PortalConfig{
			// On by default: it's the primary way to see the daemon is
			// alive without SSHing in. Still no authentication (see
			// PortalConfig's doc comment) - set enabled = false in
			// config.toml if this box isn't on a network you trust.
			Enabled: true,
			Addr:    ":8083",
			Units:   "metric",
		},
		Geocoding: GeocodingConfig{
			Enabled:   false, // opt-in: a third-party network dependency, see GeocodingConfig's doc comment
			BaseURL:   "https://nominatim.openstreetmap.org",
			UserAgent: "teslalog/0.2.0 (personal use; https://github.com/shivardev/tessieWatcher)",
		},
		API: APIConfig{
			OwnerAPIBaseURL: "https://owner-api.teslamotors.com",
			SSOBaseURL:      "https://auth.tesla.com",
			// The public client_id used for the interactive SSO PKCE login
			// flow (the only one Tesla's /oauth2/v3/authorize accepts for
			// that flow - a hex client_id found in an abandoned 2019-era
			// library turned out to be for Tesla's long-retired
			// password-grant flow instead, and is rejected outright here).
			// See runner.go/ownerapi.go for the separate, currently-under-
			// investigation question of why tokens obtained via this
			// client_id get gated to fleet-api-only on some API calls.
			ClientID:  "ownerapi",
			UserAgent: "teslalog/0.2",
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
	if d, err := parseDurationOr(raw.Polling.AsleepInterval, cfg.Polling.AsleepInterval); err != nil {
		return cfg, fmt.Errorf("polling.asleep_interval: %w", err)
	} else {
		cfg.Polling.AsleepInterval = d
	}
	if d, err := parseDurationOr(raw.Polling.DriveTimeout, cfg.Polling.DriveTimeout); err != nil {
		return cfg, fmt.Errorf("polling.drive_timeout: %w", err)
	} else {
		cfg.Polling.DriveTimeout = d
	}
	if d, err := parseDurationOr(raw.Polling.OfflineChargeMinGap, cfg.Polling.OfflineChargeMinGap); err != nil {
		return cfg, fmt.Errorf("polling.offline_charge_min_gap: %w", err)
	} else {
		cfg.Polling.OfflineChargeMinGap = d
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
	if raw.Backup.At != "" {
		if _, _, err := ParseDailyTime(raw.Backup.At); err != nil {
			return cfg, fmt.Errorf("backup.at: %w", err)
		}
		cfg.Backup.At = raw.Backup.At
	}
	if raw.Backup.RclonePath != "" {
		cfg.Backup.RclonePath = raw.Backup.RclonePath
	}
	if raw.Backup.RcloneConfig != "" {
		cfg.Backup.RcloneConfig = raw.Backup.RcloneConfig
	}
	for i, u := range raw.Backup.Upload {
		if u.Remote == "" {
			return cfg, fmt.Errorf("backup.upload[%d]: remote is required (e.g. \"googledrive:teslalog-backups\")", i)
		}
		// A remote with no colon is a local path, which would silently
		// make the "offsite" copy a second file on the same SD card -
		// the one failure mode an offsite backup exists to survive.
		if !strings.Contains(u.Remote, ":") {
			return cfg, fmt.Errorf("backup.upload[%d]: %q is not an rclone remote - it needs a remote name and a colon, e.g. \"googledrive:teslalog-backups\"", i, u.Remote)
		}
		name := u.Name
		if name == "" {
			name = strings.SplitN(u.Remote, ":", 2)[0]
		}
		cfg.Backup.Uploads = append(cfg.Backup.Uploads, UploadConfig{
			Name: name, Remote: u.Remote, RetentionDays: u.RetentionDays,
		})
	}

	if raw.Portal.Enabled != nil {
		cfg.Portal.Enabled = *raw.Portal.Enabled
	}
	if raw.Portal.Addr != "" {
		cfg.Portal.Addr = raw.Portal.Addr
	}
	if raw.Portal.Units != "" {
		switch raw.Portal.Units {
		case "metric", "imperial":
			cfg.Portal.Units = raw.Portal.Units
		default:
			return cfg, fmt.Errorf(`portal.units: must be "metric" or "imperial", got %q`, raw.Portal.Units)
		}
	}

	if raw.Geocoding.Enabled != nil {
		cfg.Geocoding.Enabled = *raw.Geocoding.Enabled
	}
	if raw.Geocoding.BaseURL != "" {
		cfg.Geocoding.BaseURL = raw.Geocoding.BaseURL
	}
	if raw.Geocoding.UserAgent != "" {
		cfg.Geocoding.UserAgent = raw.Geocoding.UserAgent
	}

	cfg.Geofences = nil
	for _, g := range raw.Geofence {
		gc := GeofenceConfig{Name: g.Name, Lat: g.Lat, Lng: g.Lng, RadiusM: g.RadiusM}
		if g.CostPerUnit != nil {
			gc.HasPricing = true
			gc.CostPerUnit = *g.CostPerUnit
			gc.SessionFee = g.SessionFee
			gc.BillingType = g.BillingType
			if gc.BillingType == "" {
				gc.BillingType = "per_kwh"
			}
			if gc.BillingType != "per_kwh" && gc.BillingType != "per_minute" {
				return cfg, fmt.Errorf("geofence %q: billing_type must be \"per_kwh\" or \"per_minute\", got %q", g.Name, g.BillingType)
			}
		}
		cfg.Geofences = append(cfg.Geofences, gc)
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
	if raw.Charging.FreeSupercharging != nil {
		cfg.Charging.FreeSupercharging = *raw.Charging.FreeSupercharging
	}

	return cfg, nil
}

func parseDurationOr(s string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	return time.ParseDuration(s)
}

// ParseDailyTime parses a local wall-clock "HH:MM" (backup.at) into
// hour and minute. Deliberately not a time.Duration: "03:00" means three
// in the morning where the car is parked, which is a different thing
// from "every 3 hours" and must not silently be read as one.
func ParseDailyTime(value string) (hour, minute int, err error) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return 0, 0, fmt.Errorf("expected a 24-hour local time like \"03:00\", got %q", value)
	}
	return parsed.Hour(), parsed.Minute(), nil
}
