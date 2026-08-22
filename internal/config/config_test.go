package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Default()
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("expected defaults for a missing file, got %+v, want %+v", cfg, want)
	}
}

func TestLoadExplicitFalseDisablesBackup(t *testing.T) {
	// Regression test: Backup.Enabled defaults to true, and an earlier
	// version of Load() merged the file's value with `||`, which meant
	// an explicit `enabled = false` in the file was silently ignored -
	// there was no way to turn backups off via config at all.
	path := writeTemp(t, "[backup]\nenabled = false\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backup.Enabled {
		t.Fatalf("backup.enabled = false in the file did not disable backups")
	}
}

func TestLoadExplicitFalseDisablesStreaming(t *testing.T) {
	// Same class of bug as above, for streaming.enabled.
	path := writeTemp(t, "[streaming]\nenabled = false\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Streaming.Enabled {
		t.Fatalf("streaming.enabled = false in the file did not disable streaming")
	}
}

func TestLoadOmittedBooleansKeepDefaults(t *testing.T) {
	path := writeTemp(t, "database = \"/tmp/custom.db\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Backup.Enabled {
		t.Fatalf("omitting backup.enabled should keep the default (true), got false")
	}
	if !cfg.Streaming.Enabled {
		t.Fatalf("omitting streaming.enabled should keep the default (true), got false")
	}
	if cfg.Database != "/tmp/custom.db" {
		t.Fatalf("database override did not apply: %q", cfg.Database)
	}
}

func TestLoadAsleepIntervalOverride(t *testing.T) {
	path := writeTemp(t, "[polling]\nasleep_interval = \"45s\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Polling.AsleepInterval != 45*time.Second {
		t.Fatalf("expected asleep_interval override to apply, got %s", cfg.Polling.AsleepInterval)
	}
}

func TestLoadAsleepIntervalDefaultsShorterThanSuspended(t *testing.T) {
	// Regression guard for the bug itself: AsleepInterval must default
	// to something well under SuspendedCheckInterval's 21 minutes, or a
	// fresh install regains the exact drive-tracking gap this was added
	// to fix. Also pins the corrected default itself (30s, matching
	// TeslaMate's real @asleep_interval - an earlier value here was a
	// guess, not verified against TeslaMate's actual source).
	cfg := Default()
	if cfg.Polling.AsleepInterval != 30*time.Second {
		t.Fatalf("expected default asleep_interval of 30s (matching TeslaMate's real @asleep_interval), got %s", cfg.Polling.AsleepInterval)
	}
	if cfg.Polling.AsleepInterval >= cfg.Polling.SuspendedCheckInterval {
		t.Fatalf("expected asleep_interval (%s) to default well under suspended_check_interval (%s)",
			cfg.Polling.AsleepInterval, cfg.Polling.SuspendedCheckInterval)
	}
}

// TestDefaultPollingMatchesTeslaMateRealDefaults pins every polling
// default that's meant to match TeslaMate's own real, verified values
// (not a memory/documentation-based guess - see each field's doc
// comment on PollingConfig) directly against the exact numbers found
// in TeslaMate's actual source. An earlier version of this project had
// IdleTimeout wrong by 5x (3m instead of 15m) and SuspendedCheckInterval
// wrong (15m instead of 21m) - both looked individually reasonable and
// both were silently incorrect until checked against ground truth.
func TestDefaultPollingMatchesTeslaMateRealDefaults(t *testing.T) {
	cfg := Default()
	cases := map[string]struct {
		got, want time.Duration
	}{
		"idle_timeout (car_settings.suspend_after_idle_min)":  {cfg.Polling.IdleTimeout, 15 * time.Minute},
		"suspended_check_interval (car_settings.suspend_min)": {cfg.Polling.SuspendedCheckInterval, 21 * time.Minute},
		"asleep_interval (@asleep_interval)":                  {cfg.Polling.AsleepInterval, 30 * time.Second},
		"drive_timeout (@drive_timeout_min)":                  {cfg.Polling.DriveTimeout, 15 * time.Minute},
	}
	for name, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: expected %s to match TeslaMate's real default, got %s", name, c.want, c.got)
		}
	}
}

func TestLoadPortalUnitsOverride(t *testing.T) {
	path := writeTemp(t, "[portal]\nunits = \"imperial\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Portal.Units != "imperial" {
		t.Fatalf("expected portal.units override to apply, got %q", cfg.Portal.Units)
	}
}

func TestLoadPortalUnitsOmittedKeepsMetricDefault(t *testing.T) {
	path := writeTemp(t, "database = \"/tmp/custom.db\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Portal.Units != "metric" {
		t.Fatalf("expected default portal.units \"metric\", got %q", cfg.Portal.Units)
	}
}

func TestLoadPortalUnitsRejectsInvalidValue(t *testing.T) {
	path := writeTemp(t, "[portal]\nunits = \"furlongs\"\n")
	if _, err := Load(path); err == nil {
		t.Fatalf("expected an error for an invalid portal.units value, got none")
	}
}

func TestLoadVehicleAndChargingOverrides(t *testing.T) {
	path := writeTemp(t, `
[vehicle]
efficiency_wh_km = 150.5

[charging]
efficiency = 0.9
price_per_kwh = 0.32
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Vehicle.EfficiencyWhKm != 150.5 {
		t.Fatalf("vehicle.efficiency_wh_km = %v, want 150.5", cfg.Vehicle.EfficiencyWhKm)
	}
	if cfg.Charging.Efficiency != 0.9 {
		t.Fatalf("charging.efficiency = %v, want 0.9", cfg.Charging.Efficiency)
	}
	if cfg.Charging.PricePerKwh != 0.32 {
		t.Fatalf("charging.price_per_kwh = %v, want 0.32", cfg.Charging.PricePerKwh)
	}
}

func TestLoadGeofencesAndGeocoding(t *testing.T) {
	path := writeTemp(t, `
[geocoding]
enabled = true
base_url = "http://localhost:9000"
user_agent = "test-agent"

[[geofence]]
name = "Home"
lat = 35.1
lng = -85.2
radius_m = 50

[[geofence]]
name = "Work"
lat = 36.0
lng = -86.0
radius_m = 100
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Geocoding.Enabled {
		t.Fatalf("expected geocoding.enabled = true")
	}
	if cfg.Geocoding.BaseURL != "http://localhost:9000" {
		t.Fatalf("unexpected base_url: %q", cfg.Geocoding.BaseURL)
	}
	if cfg.Geocoding.UserAgent != "test-agent" {
		t.Fatalf("unexpected user_agent: %q", cfg.Geocoding.UserAgent)
	}
	if len(cfg.Geofences) != 2 {
		t.Fatalf("expected 2 geofences, got %d: %+v", len(cfg.Geofences), cfg.Geofences)
	}
	if cfg.Geofences[0] != (GeofenceConfig{Name: "Home", Lat: 35.1, Lng: -85.2, RadiusM: 50}) {
		t.Fatalf("unexpected first geofence: %+v", cfg.Geofences[0])
	}
	if cfg.Geofences[1].Name != "Work" {
		t.Fatalf("unexpected second geofence: %+v", cfg.Geofences[1])
	}
}

func TestLoadNoGeofencesLeavesEmptySlice(t *testing.T) {
	path := writeTemp(t, "database = \"x.db\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Geofences) != 0 {
		t.Fatalf("expected no geofences, got %+v", cfg.Geofences)
	}
	if cfg.Geocoding.Enabled {
		t.Fatalf("expected geocoding disabled by default")
	}
}
