package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeRclone writes a shell script that records the arguments it was
// called with, so the tests assert on the command teslalog actually
// builds rather than on a mock of its own making. Skips on Windows,
// where there is no /bin/sh to stand in for rclone.
func fakeRclone(t *testing.T, exitCode int) (binary, logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell to stand in for rclone")
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	binary = filepath.Join(dir, "rclone")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	return binary, logPath
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

func calls(t *testing.T, logPath string) []string {
	t.Helper()
	body, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(body)), "\n")
}

func sampleBackup(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName(time.Now()))
	if err := os.WriteFile(path, []byte("gzip pretend"), 0o644); err != nil {
		t.Fatalf("write sample backup: %v", err)
	}
	return path
}

// TestUploadCopiesToEveryDestination is the whole point: a backup that
// only exists on the same SD card as the database it came from does not
// survive the failure it exists for.
func TestUploadCopiesToEveryDestination(t *testing.T) {
	binary, logPath := fakeRclone(t, 0)
	path := sampleBackup(t)

	u := Uploader{
		RclonePath: binary,
		ConfigPath: "",
		Destinations: []Destination{
			{Name: "Drive", Remote: "googledrive:teslalog-backups"},
			{Name: "NAS", Remote: "nas:teslalog-backups"},
		},
	}
	if err := u.Upload(context.Background(), path); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	got := calls(t, logPath)
	if len(got) != 2 {
		t.Fatalf("expected one call per destination, got %d: %v", len(got), got)
	}
	// copyto, not copy: it names the destination file explicitly, so a
	// remote path that does not exist yet is created rather than the
	// file landing under a surprise directory name.
	for i, want := range []string{
		"copyto " + path + " googledrive:teslalog-backups/" + filepath.Base(path),
		"copyto " + path + " nas:teslalog-backups/" + filepath.Base(path),
	} {
		if got[i] != want {
			t.Errorf("call %d:\n got %s\nwant %s", i, got[i], want)
		}
	}
}

// One unreachable destination must not stop the others - the copies
// exist precisely so they can fail independently.
func TestUploadContinuesAfterOneDestinationFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell to stand in for rclone")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	binary := filepath.Join(dir, "rclone")
	// Fails only for the NAS remote, succeeds otherwise.
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\ncase \"$*\" in *nas:*) exit 1 ;; esac\nexit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	path := sampleBackup(t)

	u := Uploader{
		RclonePath: binary,
		Destinations: []Destination{
			{Name: "NAS", Remote: "nas:teslalog-backups", RetentionDays: 90},
			{Name: "Drive", Remote: "googledrive:teslalog-backups"},
		},
	}
	err := u.Upload(context.Background(), path)
	if err == nil {
		t.Fatal("expected the NAS failure to be reported")
	}

	got := calls(t, logPath)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "googledrive:") {
		t.Errorf("the working destination was skipped:\n%s", joined)
	}
	// Pruning a destination this backup never reached is how a run of
	// failures silently empties the offsite copy.
	if strings.Contains(joined, "delete") {
		t.Errorf("must not prune a destination whose upload failed:\n%s", joined)
	}
}

// TestPruneIsScopedToOurOwnFiles: this runs `delete` against a directory
// a person chose, which may hold other things. It must not be able to
// empty a shared folder.
func TestPruneIsScopedToOurOwnFiles(t *testing.T) {
	binary, logPath := fakeRclone(t, 0)
	path := sampleBackup(t)

	u := Uploader{
		RclonePath:   binary,
		Destinations: []Destination{{Name: "Drive", Remote: "googledrive:backups", RetentionDays: 90}},
	}
	if err := u.Upload(context.Background(), path); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	joined := strings.Join(calls(t, logPath), "\n")
	if !strings.Contains(joined, "delete googledrive:backups --min-age 90d --include teslalog-*.db.gz") {
		t.Errorf("expected a scoped, age-bounded delete, got:\n%s", joined)
	}
}

func TestPruneSkippedWhenRetentionIsUnset(t *testing.T) {
	binary, logPath := fakeRclone(t, 0)
	path := sampleBackup(t)

	u := Uploader{
		RclonePath:   binary,
		Destinations: []Destination{{Name: "Drive", Remote: "googledrive:backups"}},
	}
	if err := u.Upload(context.Background(), path); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if joined := strings.Join(calls(t, logPath), "\n"); strings.Contains(joined, "delete") {
		t.Errorf("retention 0 means keep everything, got:\n%s", joined)
	}
}

func TestUploadPassesTheConfigPath(t *testing.T) {
	binary, logPath := fakeRclone(t, 0)
	path := sampleBackup(t)

	u := Uploader{
		RclonePath:   binary,
		ConfigPath:   "/etc/teslalog/rclone.conf",
		Destinations: []Destination{{Name: "Drive", Remote: "googledrive:backups"}},
	}
	if err := u.Upload(context.Background(), path); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	// teslalog runs as its own service user and cannot read a human's
	// ~/.config/rclone/rclone.conf, so the config path is not optional
	// in practice.
	if joined := strings.Join(calls(t, logPath), "\n"); !strings.HasPrefix(joined, "--config /etc/teslalog/rclone.conf ") {
		t.Errorf("expected --config to lead the arguments, got:\n%s", joined)
	}
}

func TestUploadReportsAMissingRclone(t *testing.T) {
	u := Uploader{
		RclonePath:   filepath.Join(t.TempDir(), "definitely-not-here"),
		Destinations: []Destination{{Name: "Drive", Remote: "googledrive:backups"}},
	}
	err := u.Upload(context.Background(), sampleBackup(t))
	if !errors.Is(err, ErrRcloneMissing) {
		t.Fatalf("expected ErrRcloneMissing, got %v", err)
	}
}

func TestUploadIsANoOpWithNoDestinations(t *testing.T) {
	if err := (Uploader{}).Upload(context.Background(), "irrelevant"); err != nil {
		t.Fatalf("expected local-only backups to be fine, got %v", err)
	}
}

// Available is checked at startup so a misconfiguration is visible then,
// rather than discovered from a missing file after the disk you needed
// the backup for has died.
func TestAvailableExplainsWhyItCannotRun(t *testing.T) {
	if ok, why := (Uploader{}).Available(); ok || !strings.Contains(why, "no offsite destinations") {
		t.Errorf("got ok=%v why=%q", ok, why)
	}

	missing := Uploader{
		RclonePath:   filepath.Join(t.TempDir(), "nope"),
		Destinations: []Destination{{Name: "Drive", Remote: "googledrive:backups"}},
	}
	if ok, why := missing.Available(); ok || !strings.Contains(why, "rclone not found") {
		t.Errorf("got ok=%v why=%q", ok, why)
	}

	binary, _ := fakeRclone(t, 0)
	unreadableConfig := Uploader{
		RclonePath:   binary,
		ConfigPath:   filepath.Join(t.TempDir(), "absent.conf"),
		Destinations: []Destination{{Name: "Drive", Remote: "googledrive:backups"}},
	}
	if ok, why := unreadableConfig.Available(); ok || !strings.Contains(why, "not readable") {
		t.Errorf("got ok=%v why=%q", ok, why)
	}
}
