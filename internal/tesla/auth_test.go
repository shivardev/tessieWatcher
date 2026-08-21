package tesla

import "testing"

// TestParseCallbackCustomScheme pins ParseCallback against the redirect
// Tesla actually uses today: a tesla://auth/callback custom URI scheme
// (RedirectURI), not the https://.../void/callback dead page every
// third-party tool used before Tesla tightened redirect_uri validation in
// ~April 2026 (github.com/teslamate-org/teslamate#5296). url.Parse handles
// arbitrary schemes fine, but this test exists so a future accidental
// regression back to assuming an "https://" callback gets caught here
// rather than only against a live Tesla account.
func TestParseCallbackCustomScheme(t *testing.T) {
	p := PKCE{State: "xyz123"}

	code, err := ParseCallback("tesla://auth/callback?code=abc789&state=xyz123&issuer=https%3A%2F%2Fauth.tesla.com%2Foauth2%2Fv3", p)
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if code != "abc789" {
		t.Fatalf("expected code %q, got %q", "abc789", code)
	}
}

func TestParseCallbackCustomSchemeStateMismatch(t *testing.T) {
	p := PKCE{State: "expected-state"}

	_, err := ParseCallback("tesla://auth/callback?code=abc789&state=wrong-state", p)
	if err == nil {
		t.Fatalf("expected a state-mismatch error, got none")
	}
}

func TestParseCallbackBareCode(t *testing.T) {
	p := PKCE{State: "xyz123"}

	code, err := ParseCallback("  abc789  \n", p)
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if code != "abc789" {
		t.Fatalf("expected code %q, got %q", "abc789", code)
	}
}
