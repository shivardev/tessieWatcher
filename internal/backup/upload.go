package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Destination is one offsite copy target, named as an rclone remote and
// path ("googledrive:teslalog-backups").
type Destination struct {
	Name          string
	Remote        string
	RetentionDays int
}

// Uploader copies finished backups offsite by shelling out to rclone.
//
// rclone rather than a native Go client, deliberately. The credentials
// that matter here already exist and already work: an rclone remote
// holds a refreshable Google OAuth token, or an SMB share's login, set
// up once by the person who owns the account. Reimplementing Google's
// consent flow inside teslalog would mean a fresh browser dance, a
// second copy of the same secret, and a token teslalog alone has to keep
// alive - all to reach a service rclone already reaches. It also means
// one code path serves Drive, SMB, S3, WebDAV and everything else rclone
// speaks, so adding a destination is a config line rather than a driver.
//
// The cost is an external binary. Treated as such: a missing rclone is
// reported once, clearly, and never fails the local backup.
type Uploader struct {
	// RclonePath is the binary to run ("rclone", or an absolute path).
	RclonePath string
	// ConfigPath is rclone's --config. teslalog runs as its own service
	// user, which cannot read a human's ~/.config/rclone/rclone.conf, so
	// this normally points at a copy that the service user owns.
	ConfigPath   string
	Destinations []Destination

	// Timeout bounds a single upload. A stalled transfer must not hold
	// the next day's backup, and on a Pi Zero over wifi a few MB can
	// legitimately take minutes.
	Timeout time.Duration
}

// ErrRcloneMissing reports that the configured rclone binary could not
// be found, so no upload is possible.
var ErrRcloneMissing = errors.New("rclone not found")

// Upload copies path to every configured destination and prunes each by
// its own retention. Returns the first error encountered, having still
// attempted every destination: one unreachable NAS must not stop the
// Drive copy, since the whole point is that the copies fail
// independently.
func (u Uploader) Upload(ctx context.Context, path string) error {
	if len(u.Destinations) == 0 {
		return nil
	}
	binary := u.RclonePath
	if binary == "" {
		binary = "rclone"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return fmt.Errorf("%w at %q: offsite backup copies are configured but cannot be made", ErrRcloneMissing, binary)
	}

	timeout := u.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}

	var firstErr error
	for _, dest := range u.Destinations {
		if err := u.copyTo(ctx, binary, timeout, dest, path); err != nil {
			slog.Error("backup upload failed", "destination", dest.Name, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			// Do not prune a destination this backup never reached:
			// pruning on a failed upload is how a run of failures
			// silently empties the offsite copy.
			continue
		}
		slog.Info("backup uploaded", "destination", dest.Name, "file", filepath.Base(path))

		if err := u.prune(ctx, binary, timeout, dest); err != nil {
			// Non-fatal: the copy that matters already landed.
			slog.Warn("backup remote pruning failed", "destination", dest.Name, "error", err)
		}
	}
	return firstErr
}

func (u Uploader) copyTo(ctx context.Context, binary string, timeout time.Duration, dest Destination, path string) error {
	// copyto, not copy: it names the destination file explicitly, so a
	// remote path that does not exist yet is created rather than the
	// file landing under a surprise directory name.
	target := strings.TrimSuffix(dest.Remote, "/") + "/" + filepath.Base(path)
	return u.run(ctx, binary, timeout, "copyto", path, target)
}

func (u Uploader) prune(ctx context.Context, binary string, timeout time.Duration, dest Destination) error {
	if dest.RetentionDays <= 0 {
		return nil // Keep everything: never inferred, always explicit.
	}
	// --min-age is age, not mtime-on-disk, which is what retention
	// means. --include guards against deleting anything at the remote
	// path that teslalog did not put there: this runs `delete` against a
	// directory a person chose, and it must not be able to empty a
	// shared folder.
	return u.run(ctx, binary, timeout, "delete", dest.Remote,
		"--min-age", fmt.Sprintf("%dd", dest.RetentionDays),
		"--include", backupGlob)
}

func (u Uploader) run(ctx context.Context, binary string, timeout time.Duration, args ...string) error {
	full := args
	if u.ConfigPath != "" {
		full = append([]string{"--config", u.ConfigPath}, args...)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, binary, full...)
	// rclone writes progress to stderr; capture it so a failure message
	// reaches the log instead of vanishing.
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if len(message) > 500 {
			message = message[:500] + "..."
		}
		// Never include `full` in the error: an inline remote definition
		// can carry a password, and this string reaches the log.
		return fmt.Errorf("rclone %s: %w: %s", args[0], err, message)
	}
	return nil
}

// Available reports whether uploads can run at all, with a reason when
// they cannot. Checked at startup so a misconfiguration is visible then
// rather than at 03:00 the next morning.
func (u Uploader) Available() (bool, string) {
	if len(u.Destinations) == 0 {
		return false, "no offsite destinations configured"
	}
	binary := u.RclonePath
	if binary == "" {
		binary = "rclone"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return false, fmt.Sprintf("rclone not found at %q", binary)
	}
	if u.ConfigPath != "" {
		if _, err := os.Stat(u.ConfigPath); err != nil {
			return false, fmt.Sprintf("rclone config %s is not readable by this user: %v", u.ConfigPath, err)
		}
	}
	return true, ""
}
