package tesla

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"teslalog/internal/config"
)

// memTokenStore is an in-memory TokenStore standing in for a shared token
// file, so tests can simulate a second process (e.g. `teslalog status`
// running alongside the `teslalog run` daemon) having already refreshed
// and persisted a fresh token.
type memTokenStore struct {
	ts TokenSet
}

func (m *memTokenStore) Load() (TokenSet, error) { return m.ts, nil }
func (m *memTokenStore) Save(ts TokenSet) error  { m.ts = ts; return nil }

func TestEnsureFreshTokenReloadsBeforeRefreshing(t *testing.T) {
	// A refresh endpoint that must NOT be hit: if it is, the race-avoidance
	// reload didn't happen and the test should fail.
	refreshCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	store := &memTokenStore{ts: TokenSet{
		AccessToken: "on-disk-fresh-token", RefreshToken: "refresh-1",
		ExpiresAt: time.Now().Add(time.Hour), // another process already refreshed this
	}}

	client := &Client{
		http:  &http.Client{Timeout: 5 * time.Second},
		api:   config.APIConfig{SSOBaseURL: server.URL, ClientID: "ownerapi", UserAgent: "test"},
		store: store,
		// This Client's own in-memory copy is stale/expired, as if it
		// loaded its tokens before the other process's refresh landed.
		tokens: TokenSet{AccessToken: "stale-token", RefreshToken: "refresh-1", ExpiresAt: time.Now().Add(-time.Minute)},
	}

	if err := client.ensureFreshToken(context.Background()); err != nil {
		t.Fatalf("ensureFreshToken: %v", err)
	}
	if refreshCalls != 0 {
		t.Fatalf("expected the refresh endpoint to never be called, got %d calls", refreshCalls)
	}
	if client.tokens.AccessToken != "on-disk-fresh-token" {
		t.Fatalf("expected client to adopt the fresher on-disk token, got %q", client.tokens.AccessToken)
	}
}

func TestEnsureFreshTokenAdoptsOnDiskRefreshTokenBeforeRefreshing(t *testing.T) {
	// Both the client's in-memory copy AND the on-disk copy are
	// access-token-expired, but the on-disk refresh token is newer (as if
	// another process rotated it and then itself failed/raced) - the
	// actual refresh call must use the on-disk refresh token, not the
	// client's stale in-memory one.
	var gotRefreshToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotRefreshToken = r.FormValue("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"new-token","refresh_token":"refresh-2-new","expires_in":3600}`))
	}))
	defer server.Close()

	store := &memTokenStore{ts: TokenSet{
		AccessToken: "on-disk-expired-too", RefreshToken: "refresh-2",
		ExpiresAt: time.Now().Add(-time.Minute),
	}}

	client := &Client{
		http:   &http.Client{Timeout: 5 * time.Second},
		api:    config.APIConfig{SSOBaseURL: server.URL, ClientID: "ownerapi", UserAgent: "test"},
		store:  store,
		tokens: TokenSet{AccessToken: "stale-token", RefreshToken: "refresh-1-old", ExpiresAt: time.Now().Add(-time.Hour)},
	}

	if err := client.ensureFreshToken(context.Background()); err != nil {
		t.Fatalf("ensureFreshToken: %v", err)
	}
	if gotRefreshToken != "refresh-2" {
		t.Fatalf("expected refresh to use the on-disk refresh token %q, got %q", "refresh-2", gotRefreshToken)
	}
	if client.tokens.AccessToken != "new-token" {
		t.Fatalf("expected client to hold the newly refreshed token, got %q", client.tokens.AccessToken)
	}
	if store.ts.AccessToken != "new-token" {
		t.Fatalf("expected the new token to be persisted back to the store")
	}
}

// TestListVehiclesUsesProductsEndpointAndFiltersNonVehicles pins two
// things that broke teslalog against a real account: (1) ListVehicles
// must call /api/1/products, not the older /api/1/vehicles (which Tesla
// gates to "Endpoint is only available on fleetapi" as of 2026, even
// though the rest of the legacy Owner API - including vehicle_data -
// still works); (2) /api/1/products also lists non-vehicle account
// products (Powerwalls, energy sites) interleaved in the same array,
// which must be filtered out rather than returned as bogus vehicles.
func TestListVehiclesUsesProductsEndpointAndFiltersNonVehicles(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"response": [
				{"id": 1, "vehicle_id": 42, "vin": "5YJ3E1EA1PF000001", "display_name": "My Model 3", "state": "online", "in_service": false},
				{"id": 2, "energy_site_id": 99, "resource_type": "battery", "site_name": "My Powerwall"}
			],
			"count": 2
		}`))
	}))
	defer server.Close()

	client := &Client{
		http:   &http.Client{Timeout: 5 * time.Second},
		api:    config.APIConfig{OwnerAPIBaseURL: server.URL, UserAgent: "test"},
		store:  &memTokenStore{ts: TokenSet{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}},
		tokens: TokenSet{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)},
	}

	vehicles, err := client.ListVehicles(context.Background())
	if err != nil {
		t.Fatalf("ListVehicles: %v", err)
	}
	if gotPath != "/api/1/products" {
		t.Fatalf("expected a request to /api/1/products, got %q", gotPath)
	}
	if len(vehicles) != 1 {
		t.Fatalf("expected exactly 1 vehicle (Powerwall filtered out), got %d", len(vehicles))
	}
	if vehicles[0].VIN != "5YJ3E1EA1PF000001" || vehicles[0].VehicleID != 42 {
		t.Fatalf("unexpected vehicle: %+v", vehicles[0])
	}
}

// TestVehicleDataRequestsAllEndpoints pins VehicleData to send the exact
// same endpoints= query parameter TeslaMate's own client sends
// (lib/tesla_api/vehicle.ex) - without it, Tesla's vehicle_data endpoint
// has been observed to silently omit response sections rather than
// error, which would corrupt data (e.g. missing vehicle_config breaking
// model/trim identification) without ever surfacing as a request failure.
func TestVehicleDataRequestsAllEndpoints(t *testing.T) {
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":{}}`))
	}))
	defer server.Close()

	client := &Client{
		http:   &http.Client{Timeout: 5 * time.Second},
		api:    config.APIConfig{OwnerAPIBaseURL: server.URL, UserAgent: "test"},
		store:  &memTokenStore{ts: TokenSet{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}},
		tokens: TokenSet{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)},
	}

	if _, _, err := client.VehicleData(context.Background(), 555); err != nil {
		t.Fatalf("VehicleData: %v", err)
	}

	want := "charge_state;climate_state;closures_state;drive_state;gui_settings;location_data;vehicle_config;vehicle_state;vehicle_data_combo"
	if got := gotQuery.Get("endpoints"); got != want {
		t.Fatalf("expected endpoints=%q, got %q", want, got)
	}
}
