package main

import "testing"

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
