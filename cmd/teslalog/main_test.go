package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
