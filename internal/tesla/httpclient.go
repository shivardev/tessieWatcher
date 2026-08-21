package tesla

import (
	"crypto/tls"
	"net/http"
	"time"
)

// NewHardenedClient returns an *http.Client configured to negotiate
// TLS 1.3 (and, via Go's automatic HTTP/2-over-ALPN upgrade - see
// below, unaffected by setting just MinVersion here) for every
// request it makes.
//
// Why this exists: Tesla's edge has been observed (independently, by
// several unrelated projects) fingerprinting the TLS handshake used
// to mint/refresh an OAuth token at auth.tesla.com, and rejecting
// *every subsequent API call made with that token* - not just the
// minting request itself - with HTTP 403
// {"error":"forbidden, see https://developer.tesla.com/docs/fleet-api"}
// regardless of which client or endpoint makes the later call. This
// is misleading (it looks like an endpoint got Fleet-API-gated, or
// like the token expired) but is neither - TeslaMate's maintainers
// hit and fixed the identical symptom this way in June 2026
// (teslamate-org/teslamate#5399, fixed by #5406: "enable HTTP/2 and
// set TLS to 1.3 for TESLA_AUTH_HOST"), and a Home Assistant
// integration maintainer (alandtse/tesla#1200-#1202) described the
// mechanism directly: "the 403 is not the API dying and not the token
// being invalid, it is Tesla refusing the client that minted it."
//
// Go's http.Transport already defaults to TLS 1.3 when both ends
// support it, and auto-negotiates HTTP/2 via ALPN whenever
// TLSClientConfig.NextProtos is left unset (which it is here) - so
// setting MinVersion explicitly is mostly a hedge against some future
// Go default change, not itself the fix. If Tesla's edge starts
// fingerprinting more than TLS version/ALPN (cipher suite order,
// extensions, etc.), this alone won't be enough - see this function's
// call sites' comments for what to check next if the symptom returns.
func NewHardenedClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13},
		},
	}
}
