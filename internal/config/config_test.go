package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
