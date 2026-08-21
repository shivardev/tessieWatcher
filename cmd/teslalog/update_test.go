package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestAssetNameForEveryBuiltPlatform pins assetNameFor's output to
// exactly what deploy/cross-build.sh produces for every platform it
// builds. A mismatch here means `teslalog update` would silently
// download a binary for the wrong CPU/OS - it wouldn't error until exec
// time on whatever device ran it (e.g. a Pi Zero 2 W in the field).
func TestAssetNameForEveryBuiltPlatform(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"linux", "amd64", "teslalog-linux-amd64"},
		{"linux", "arm64", "teslalog-linux-arm64"},
		{"linux", "arm", "teslalog-linux-armv7"},
		{"windows", "amd64", "teslalog-windows-amd64.exe"},
	}
	for _, c := range cases {
		got, err := assetNameFor(c.goos, c.goarch)
		if err != nil {
			t.Fatalf("%s/%s: unexpected error: %v", c.goos, c.goarch, err)
		}
		if got != c.want {
			t.Fatalf("%s/%s: expected asset %q, got %q", c.goos, c.goarch, c.want, got)
		}
	}
}

func TestAssetNameForUnsupportedPlatformErrors(t *testing.T) {
	cases := []struct{ goos, goarch string }{
		{"darwin", "arm64"}, // no macOS build exists - must error, not guess
		{"linux", "386"},
		{"windows", "arm64"},
	}
	for _, c := range cases {
		if _, err := assetNameFor(c.goos, c.goarch); err == nil {
			t.Fatalf("%s/%s: expected an error for a platform with no prebuilt binary", c.goos, c.goarch)
		}
	}
}

// TestDownloadFileToUnwritableTargetReturnsPermissionError pins the
// error runUpdate's "run as root instead" hint (see update.go) relies
// on: writing to a path this process can't write to must produce an
// error that errors.Is(err, fs.ErrPermission) recognizes - this is
// exactly what happens for real when `teslalog update` runs as the
// unprivileged teslalog service user against a root-owned install
// (e.g. /usr/local/bin - see deploy/install.sh), which is the bug this
// hint exists for.
func TestDownloadFileToUnwritableTargetReturnsPermissionError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "teslalog.new")

	// Create the file first, then strip write permission - downloadFile
	// always opens with O_CREATE|O_TRUNC, so an existing read-only file
	// (rather than an unwritable parent directory, which behaves
	// inconsistently across OSes in a temp-dir test) reliably exercises
	// the same "can't write here" failure on both Windows and Linux.
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}
	if err := os.Chmod(target, 0o444); err != nil {
		t.Fatalf("chmod read-only: %v", err)
	}
	t.Cleanup(func() { os.Chmod(target, 0o644) }) // let TempDir clean up

	// The exact open call downloadFile makes on its destination path.
	_, openErr := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if !errors.Is(openErr, fs.ErrPermission) {
		t.Fatalf("expected fs.ErrPermission opening a read-only file for writing, got %v", openErr)
	}
}
