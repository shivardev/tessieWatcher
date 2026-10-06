// update.go implements `teslalog update`: check GitHub Releases for a
// newer version and replace the current binary in place, with no git
// clone or manual download required. This is the "quick updates without
// cloning" mechanism - see README's Updating section.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// repoSlug is this project's GitHub repo, used to check for and download
// releases. Update this if the repo is ever renamed/moved.
const repoSlug = "shivardev/tessieWatcher"

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// compareVersions compares the numeric major/minor/patch portion of two
// release versions. It prevents an older GitHub release from replacing a
// newer locally built binary (for example v0.6.0 with GitHub's v0.5.1).
func compareVersions(a, b string) (int, error) {
	parse := func(value string) ([3]int, error) {
		var result [3]int
		value = strings.TrimPrefix(strings.TrimSpace(value), "v")
		value = strings.SplitN(value, "+", 2)[0]
		value = strings.SplitN(value, "-", 2)[0]
		parts := strings.Split(value, ".")
		if len(parts) != 3 {
			return result, fmt.Errorf("invalid version %q (expected major.minor.patch)", value)
		}
		for i, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 {
				return result, fmt.Errorf("invalid version %q", value)
			}
			result[i] = n
		}
		return result, nil
	}
	av, err := parse(a)
	if err != nil {
		return 0, err
	}
	bv, err := parse(b)
	if err != nil {
		return 0, err
	}
	for i := range av {
		if av[i] < bv[i] {
			return -1, nil
		}
		if av[i] > bv[i] {
			return 1, nil
		}
	}
	return 0, nil
}

// assetNameForPlatform returns the release-asset filename cross-build.sh
// produces for the platform teslalog itself is currently running on.
func assetNameForPlatform() (string, error) {
	return assetNameFor(runtime.GOOS, runtime.GOARCH)
}

// assetNameFor is assetNameForPlatform's actual logic, taking goos/goarch
// as parameters so it's unit-testable without cross-compiling: getting
// this wrong means silently downloading a binary for the wrong CPU/OS,
// which fails at exec time (e.g. "exec format error"), not at download
// time - exactly the kind of mismatch this project already hit tonight
// running a linux binary from a Windows shell.
func assetNameFor(goos, goarch string) (string, error) {
	switch goos {
	case "linux":
		switch goarch {
		case "amd64":
			return "teslalog-linux-amd64", nil
		case "arm64":
			return "teslalog-linux-arm64", nil
		case "arm":
			return "teslalog-linux-armv7", nil
		}
	case "windows":
		if goarch == "amd64" {
			return "teslalog-windows-amd64.exe", nil
		}
	}
	return "", fmt.Errorf("no prebuilt binary for %s/%s - build from source instead (see README)", goos, goarch)
}

func runUpdate() error {
	client := &http.Client{Timeout: 30 * time.Second}

	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+repoSlug+"/releases/latest", nil)
	if err != nil {
		return err
	}
	// GitHub's API rejects unauthenticated requests with no User-Agent.
	req.Header.Set("User-Agent", "teslalog-updater/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("check latest release: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read release info: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("check latest release: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return fmt.Errorf("decode release info: %w", err)
	}

	latest := strings.TrimPrefix(rel.TagName, "v")
	if latest == "" {
		return fmt.Errorf("no releases found for %s yet", repoSlug)
	}
	comparison, err := compareVersions(latest, version)
	if err != nil {
		return fmt.Errorf("compare installed and release versions: %w", err)
	}
	if comparison == 0 {
		fmt.Printf("Already up to date (v%s).\n", version)
		return nil
	}
	if comparison < 0 {
		fmt.Printf("Installed v%s is newer than the latest GitHub release (%s); refusing to downgrade.\n", version, rel.TagName)
		return nil
	}

	assetName, err := assetNameForPlatform()
	if err != nil {
		return err
	}
	var downloadURL string
	for _, a := range rel.Assets {
		if a.Name == assetName {
			downloadURL = a.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		return fmt.Errorf("release %s has no %q asset", rel.TagName, assetName)
	}

	fmt.Printf("Updating teslalog v%s -> %s ...\n", version, rel.TagName)

	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find current executable: %w", err)
	}
	selfPath, err = filepath.EvalSymlinks(selfPath)
	if err != nil {
		return fmt.Errorf("resolve current executable path: %w", err)
	}

	tmpPath := selfPath + ".new"
	if err := downloadFile(client, downloadURL, tmpPath); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			// The common case: teslalog is installed to a root-owned
			// directory (/usr/local/bin - see deploy/install.sh), but
			// this process is running as the unprivileged 'teslalog'
			// service user, which can't write a new file there. Unlike
			// `auth` (which must run as the teslalog user, so token file
			// ownership matches the daemon), `update` replaces a system
			// binary and needs root - see the README's Updating section.
			return fmt.Errorf("download %s: %w\n\nThis usually means teslalog needs to run as root to replace its own binary "+
				"(try: sudo teslalog update ...), not as the unprivileged teslalog service user.", assetName, err)
		}
		return fmt.Errorf("download %s: %w", assetName, err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil { // no-op on Windows, required on Linux
		os.Remove(tmpPath)
		return fmt.Errorf("make new binary executable: %w", err)
	}

	if err := os.Rename(tmpPath, selfPath); err != nil {
		// Expected on Windows: you can't overwrite a running .exe. Leave
		// the downloaded file in place and tell the user how to finish.
		fmt.Printf("Downloaded to %s, but couldn't replace the running binary in place (%v).\n", tmpPath, err)
		fmt.Println("Stop teslalog, then manually replace it with that file and restart.")
		return nil
	}

	fmt.Printf("Updated to %s. Tokens/database are untouched - no need to log in again.\n", rel.TagName)
	fmt.Println("If running as a service: sudo systemctl restart teslalog")
	return nil
}

func downloadFile(client *http.Client, url, dstPath string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "teslalog-updater/"+version)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(dstPath)
		return err
	}
	return nil
}
