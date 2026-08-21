package tesla

import (
	"crypto/tls"
	"net/http"
	"testing"
	"time"
)

// TestNewHardenedClientForcesTLS13 pins the one thing NewHardenedClient
// exists for - see its doc comment for why. This is intentionally a
// narrow, mechanical test (it can't verify Tesla's edge actually
// accepts the resulting fingerprint - only a live call can) but it
// guards against a future refactor accidentally dropping the
// TLSClientConfig and silently reverting to Go's plain defaults.
func TestNewHardenedClientForcesTLS13(t *testing.T) {
	c := NewHardenedClient(5 * time.Second)

	if c.Timeout != 5*time.Second {
		t.Fatalf("expected the requested timeout to be preserved, got %v", c.Timeout)
	}

	transport, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected an *http.Transport, got %T", c.Transport)
	}
	if transport.TLSClientConfig == nil {
		t.Fatalf("expected a non-nil TLSClientConfig")
	}
	if transport.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("expected MinVersion TLS 1.3, got %x", transport.TLSClientConfig.MinVersion)
	}
}
