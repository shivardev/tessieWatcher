// Package tesla talks to Tesla's unofficial Owner API and the legacy
// streaming websocket. It is deliberately isolated from storage/ and
// vehicle/ so that if Tesla ever kills the Owner API, only this
// directory needs to be replaced (e.g. with a Fleet API client) — the
// SQLite schema, sleep/state logic, backups and CLI stay intact.
//
// auth.go implements Tesla's SSO PKCE login flow: the same flow used
// by the official Tesla mobile app, and the one TeslaMate, teslapy and
// tesla_auth use today now that Tesla retired the old
// username/password "password" OAuth grant. There is no way to
// authenticate non-interactively without a browser — Tesla's login
// page can require 2FA/CAPTCHA — so `teslalog auth` prints a URL for
// the user to open, and reads back the resulting redirect.
package tesla

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"teslalog/internal/config"
)

// RedirectURI is the callback Tesla's SSO redirects the browser to after
// login, with ?code=...&state=... attached. It does not need to resolve
// to anything real — the user copies the resulting redirect back into
// `teslalog auth` rather than the browser ever completing it.
//
// This is a custom URI scheme (what the real Tesla mobile app registers
// itself as the OS handler for), not the "https://auth.tesla.com/void/callback"
// dead-page URL every third-party tool (TeslaMate, teslapy, tesla_auth,
// and this project) used to use. Tesla started rejecting that redirect_uri
// for the "ownerapi" client_id in ~April 2026 ("The 'redirect_uri' supplied
// is not registered for this 'client_id'" — see
// github.com/teslamate-org/teslamate#5296); tesla_auth v0.13.0+ fixed it by
// switching to this exact value, which is still registered for
// "ownerapi". client_id itself is unaffected by that change.
const RedirectURI = "tesla://auth/callback"

const authScope = "openid email offline_access"

// PKCE holds a PKCE code verifier/challenge pair and the anti-CSRF
// state value for one login attempt.
type PKCE struct {
	Verifier  string
	Challenge string
	State     string
}

// NewPKCE generates a fresh code_verifier/code_challenge/state triple.
func NewPKCE() (PKCE, error) {
	verifier, err := randomURLSafe(86)
	if err != nil {
		return PKCE{}, err
	}
	state, err := randomURLSafe(16)
	if err != nil {
		return PKCE{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return PKCE{Verifier: verifier, Challenge: challenge, State: state}, nil
}

func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n], nil
}

// AuthorizeURL builds the URL the user must open in a browser to log
// in to their Tesla account.
func AuthorizeURL(api config.APIConfig, p PKCE) string {
	v := url.Values{}
	v.Set("client_id", api.ClientID)
	v.Set("code_challenge", p.Challenge)
	v.Set("code_challenge_method", "S256")
	v.Set("redirect_uri", RedirectURI)
	v.Set("response_type", "code")
	v.Set("scope", authScope)
	v.Set("state", p.State)
	v.Set("locale", "en-US")
	return strings.TrimSuffix(api.SSOBaseURL, "/") + "/oauth2/v3/authorize?" + v.Encode()
}

// ParseCallback extracts the authorization code from either a bare
// code or the full URL the browser landed on after login
// (https://auth.tesla.com/void/callback?code=...&state=...). It
// validates the state matches p.State when a full URL was supplied.
func ParseCallback(input string, p PKCE) (code string, err error) {
	input = strings.TrimSpace(input)
	if !strings.Contains(input, "://") && !strings.Contains(input, "?") {
		// Looks like a bare code already.
		return input, nil
	}

	u, err := url.Parse(input)
	if err != nil {
		return "", fmt.Errorf("parse redirect URL: %w", err)
	}
	q := u.Query()
	code = q.Get("code")
	if code == "" {
		return "", fmt.Errorf("no ?code= parameter found in %q", input)
	}
	if state := q.Get("state"); state != "" && p.State != "" && state != p.State {
		return "", fmt.Errorf("state mismatch: got %q, expected %q (possible CSRF, try logging in again)", state, p.State)
	}
	return code, nil
}

// TokenSet is what we persist to the token file.
type TokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Expired reports whether the access token is expired or within the
// given safety margin of expiring.
func (t TokenSet) Expired(margin time.Duration) bool {
	if t.AccessToken == "" {
		return true
	}
	return time.Now().Add(margin).After(t.ExpiresAt)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// ExchangeCode trades an authorization code (from ParseCallback) for a
// TokenSet.
func ExchangeCode(httpClient *http.Client, api config.APIConfig, p PKCE, code string) (TokenSet, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", api.ClientID)
	form.Set("code", code)
	form.Set("code_verifier", p.Verifier)
	form.Set("redirect_uri", RedirectURI)
	return doTokenRequest(httpClient, api, form)
}

// RefreshAccessToken exchanges a refresh token for a new TokenSet.
func RefreshAccessToken(httpClient *http.Client, api config.APIConfig, refreshToken string) (TokenSet, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", api.ClientID)
	form.Set("refresh_token", refreshToken)
	form.Set("scope", authScope)
	return doTokenRequest(httpClient, api, form)
}

func doTokenRequest(httpClient *http.Client, api config.APIConfig, form url.Values) (TokenSet, error) {
	endpoint := strings.TrimSuffix(api.SSOBaseURL, "/") + "/oauth2/v3/token"
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", api.UserAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return TokenSet{}, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return TokenSet{}, fmt.Errorf("read token response: %w", err)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return TokenSet{}, fmt.Errorf("decode token response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		if tr.Error != "" {
			return TokenSet{}, fmt.Errorf("token exchange failed: %s: %s", tr.Error, tr.ErrorDesc)
		}
		return TokenSet{}, fmt.Errorf("token exchange failed: HTTP %d: %s", resp.StatusCode, string(body))
	}

	return TokenSet{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

// LoadTokenFile reads a TokenSet from disk.
func LoadTokenFile(path string) (TokenSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TokenSet{}, err
	}
	var ts TokenSet
	if err := json.Unmarshal(data, &ts); err != nil {
		return TokenSet{}, fmt.Errorf("parse token file %s: %w", path, err)
	}
	return ts, nil
}

// SaveTokenFile writes a TokenSet to disk with 0600 permissions (it
// contains long-lived credentials, not a Tesla password, but still
// sensitive).
func SaveTokenFile(path string, ts TokenSet) error {
	data, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write token file %s: %w", path, err)
	}
	return nil
}
