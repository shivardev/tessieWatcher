package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"teslalog/internal/storage"
)

// withPipeStdin temporarily replaces os.Stdin with a pipe teasts can write
// to (or not), restoring the original on cleanup.
func withPipeStdin(t *testing.T) (w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		r.Close()
	})
	return w
}

// TestWaitForCallbackRelayWins covers the auto-capture path: a
// `teslalog auth-callback` invocation (simulated here by directly writing
// the relay file, since that's all runAuthCallback itself does) unblocks
// waitForCallback even though nothing was ever typed at stdin.
func TestWaitForCallbackRelayWins(t *testing.T) {
	relayPath := filepath.Join(t.TempDir(), "relay.txt")

	stdinW := withPipeStdin(t) // never written to - stdin just sits open, like a real terminal
	defer stdinW.Close()

	go func() {
		time.Sleep(100 * time.Millisecond)
		if err := os.WriteFile(relayPath, []byte("tesla://auth/callback?code=abc123&state=xyz"), 0o600); err != nil {
			t.Errorf("write relay file: %v", err)
		}
	}()

	line, err := waitForCallback(relayPath)
	if err != nil {
		t.Fatalf("waitForCallback: %v", err)
	}
	if !strings.Contains(line, "code=abc123") {
		t.Fatalf("expected the relayed URL, got %q", line)
	}
}

// TestWaitForCallbackStdinWins covers the original, universal path: a
// human pasting the redirect (or just the code) into the terminal, with
// no protocol handler ever registered (relayPath never gets created).
func TestWaitForCallbackStdinWins(t *testing.T) {
	relayPath := filepath.Join(t.TempDir(), "relay-never-created.txt")

	stdinW := withPipeStdin(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		stdinW.Write([]byte("pasted-code-here\n"))
	}()

	line, err := waitForCallback(relayPath)
	if err != nil {
		t.Fatalf("waitForCallback: %v", err)
	}
	if strings.TrimSpace(line) != "pasted-code-here" {
		t.Fatalf("expected the pasted line, got %q", line)
	}
}

// TestAuthCallbackRelayPathRoundTrip pins runAuthCallback and
// authCallbackRelayPath together: what one writes, the other's path
// must find.
func TestAuthCallbackRelayPathRoundTrip(t *testing.T) {
	// authCallbackRelayPath is a fixed path (os.TempDir()-based), so this
	// just proves runAuthCallback writes exactly what was passed in,
	// readable back from that fixed path.
	relayPath := authCallbackRelayPath()
	defer os.Remove(relayPath)

	url := "tesla://auth/callback?code=roundtrip-test&state=s1"
	if err := runAuthCallback([]string{url}); err != nil {
		t.Fatalf("runAuthCallback: %v", err)
	}

	data, err := os.ReadFile(relayPath)
	if err != nil {
		t.Fatalf("read relay file: %v", err)
	}
	if string(data) != url {
		t.Fatalf("expected relay file to contain %q, got %q", url, string(data))
	}
}

func TestRunAuthCallbackRequiresArg(t *testing.T) {
	if err := runAuthCallback(nil); err == nil {
		t.Fatalf("expected an error with no URL argument")
	}
}

// TestRunExportIncludesDerivedColumns seeds one drive and one DC charge
// directly through the storage layer, then runs the real "export"
// command against it and checks the CSV carries the derived
// efficiency/AC-DC columns (not just the raw ones) - these are computed
// in internal/storage, not in runExport itself, so this pins the wiring
// between the two rather than re-testing the math.
func TestRunExportIncludesDerivedColumns(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	vehicleID, err := store.UpsertVehicle(storage.VehicleMeta{VIN: "EXPORTVIN", TeslaID: "1", DisplayName: "Car"})
	if err != nil {
		t.Fatalf("upsert vehicle: %v", err)
	}

	now := time.Now().UTC()
	driveID, err := store.OpenDrive(storage.DriveStart{VehicleID: vehicleID, Time: now, OdometerKm: 0, RangeKm: 300})
	if err != nil {
		t.Fatalf("open drive: %v", err)
	}
	if err := store.CloseDrive(storage.DriveEnd{DriveID: driveID, Time: now.Add(10 * time.Minute), OdometerKm: 10, RangeKm: 290}); err != nil {
		t.Fatalf("close drive: %v", err)
	}

	chargeID, err := store.OpenChargingSession(storage.ChargeStart{VehicleID: vehicleID, Time: now, BatteryLevel: 20, RangeKm: 80})
	if err != nil {
		t.Fatalf("open charge: %v", err)
	}
	if err := store.AppendChargingSample(storage.ChargingSample{
		ChargingSessionID: chargeID, VehicleID: vehicleID, Time: now.Add(time.Minute), FastChargerPresent: true,
	}); err != nil {
		t.Fatalf("append charging sample: %v", err)
	}
	if err := store.CloseChargingSession(storage.ChargeEnd{ChargingSessionID: chargeID, Time: now.Add(20 * time.Minute), BatteryLevel: 80, RangeKm: 320, EnergyAddedKwh: 40}); err != nil {
		t.Fatalf("close charge: %v", err)
	}
	store.Close()

	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`database = "`+filepath.ToSlash(dbPath)+`"`+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	drivesOut := filepath.Join(dir, "drives.csv")
	if err := runExport(cfgPath, []string{"drives", "-out", drivesOut}); err != nil {
		t.Fatalf("runExport drives: %v", err)
	}
	drivesCSV, err := os.ReadFile(drivesOut)
	if err != nil {
		t.Fatalf("read drives csv: %v", err)
	}
	for _, want := range []string{"rated_range_lost_km", "efficiency_ratio", "10.00", "1.000"} {
		if !strings.Contains(string(drivesCSV), want) {
			t.Fatalf("expected drives.csv to contain %q, got:\n%s", want, drivesCSV)
		}
	}

	chargesOut := filepath.Join(dir, "charges.csv")
	if err := runExport(cfgPath, []string{"charges", "-out", chargesOut}); err != nil {
		t.Fatalf("runExport charges: %v", err)
	}
	chargesCSV, err := os.ReadFile(chargesOut)
	if err != nil {
		t.Fatalf("read charges csv: %v", err)
	}
	for _, want := range []string{"charge_type", "kwh_per_rated_km", "DC"} {
		if !strings.Contains(string(chargesCSV), want) {
			t.Fatalf("expected charges.csv to contain %q, got:\n%s", want, chargesCSV)
		}
	}
}
