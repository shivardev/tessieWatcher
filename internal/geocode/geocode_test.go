package geocode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// memCache is a trivial in-memory Cache for tests.
type memCache struct {
	m map[[2]float64]Place
}

func newMemCache() *memCache { return &memCache{m: map[[2]float64]Place{}} }

func (c *memCache) Lookup(latKey, lngKey float64) (Place, bool, error) {
	place, ok := c.m[[2]float64{latKey, lngKey}]
	return place, ok, nil
}

func (c *memCache) Save(latKey, lngKey float64, place Place) error {
	c.m[[2]float64{latKey, lngKey}] = place
	return nil
}

func TestResolveMatchesGeofenceBeforeAnythingElse(t *testing.T) {
	geofences := []Geofence{{Name: "Home", Lat: 35.0000, Lng: -85.0000, RadiusM: 100}}
	r := New(geofences, nil, true, "http://should-not-be-called.invalid", "test")

	// A point ~50m from "Home" (well within its 100m radius).
	got := r.Resolve(context.Background(), 35.00035, -85.0000)
	if got != "Home" {
		t.Fatalf("expected geofence match 'Home', got %q", got)
	}
}

// TestResolvePicksTheNearestOverlappingGeofence pins TeslaMate's own
// find_geofence behavior (verified directly against
// lib/teslamate/locations.ex: it orders candidates by distance and
// takes the closest). teslalog used to return whichever matching
// geofence appeared first in config, so with a small zone nested
// inside a larger one - e.g. "Home garage" inside "Home" - the
// reported location depended on how the config file happened to be
// ordered rather than on where the car actually was.
func TestResolvePicksTheNearestOverlappingGeofence(t *testing.T) {
	// Deliberately listed largest-first, so a naive first-match would
	// return the wrong (less specific) one.
	geofences := []Geofence{
		{Name: "Home", Lat: 35.0000, Lng: -85.0000, RadiusM: 500},
		{Name: "Home garage", Lat: 35.0002, Lng: -85.0000, RadiusM: 50},
	}
	r := New(geofences, nil, false, "", "test")

	// Sitting essentially on top of the garage - inside both zones.
	if got := r.Resolve(context.Background(), 35.00021, -85.0000); got != "Home garage" {
		t.Fatalf("expected the nearest (most specific) geofence 'Home garage', got %q", got)
	}

	// Elsewhere in the yard: inside "Home" only.
	if got := r.Resolve(context.Background(), 35.0030, -85.0000); got != "Home" {
		t.Fatalf("expected 'Home' when outside the garage's radius, got %q", got)
	}
}

// TestGeofenceCost pins TeslaMate's put_cost semantics (verified
// directly against lib/teslamate/log.ex): per-kWh billing charges for
// the GREATER of energy added and energy drawn from the wall,
// per-minute billing charges for elapsed time, and either way a flat
// session fee is added on top. A zone with no pricing configured
// reports no cost at all rather than a misleading zero.
func TestGeofenceCost(t *testing.T) {
	cases := []struct {
		name        string
		g           Geofence
		added, used float64
		durationMin float64
		wantCost    float64
		wantOK      bool
	}{
		{
			name:  "per kWh bills the greater of added and used",
			g:     Geofence{HasPricing: true, BillingType: "per_kwh", CostPerUnit: 0.125},
			added: 30, used: 33, // 33 kWh actually drawn from the wall
			wantCost: 33 * 0.125, wantOK: true,
		},
		{
			name:  "per kWh falls back to added when used is unavailable",
			g:     Geofence{HasPricing: true, BillingType: "per_kwh", CostPerUnit: 0.125},
			added: 30, used: 0,
			wantCost: 30 * 0.125, wantOK: true,
		},
		{
			name:  "session fee is added on top",
			g:     Geofence{HasPricing: true, BillingType: "per_kwh", CostPerUnit: 0.30, SessionFee: 0.45},
			added: 10, used: 11,
			wantCost: 0.45 + 11*0.30, wantOK: true,
		},
		{
			name:  "a free charger costs zero, and that's a real answer",
			g:     Geofence{HasPricing: true, BillingType: "per_kwh", CostPerUnit: 0},
			added: 40, used: 44,
			wantCost: 0, wantOK: true,
		},
		{
			name:  "per minute bills elapsed time",
			g:     Geofence{HasPricing: true, BillingType: "per_minute", CostPerUnit: 0.05, SessionFee: 1},
			added: 10, used: 11,
			durationMin: 60,
			wantCost:    1 + 60*0.05, wantOK: true,
		},
		{
			name:  "an empty billing type defaults to per kWh",
			g:     Geofence{HasPricing: true, CostPerUnit: 0.2},
			added: 10, used: 10,
			wantCost: 2, wantOK: true,
		},
		{
			name:  "no pricing configured reports no cost, not zero",
			g:     Geofence{Name: "Somewhere"},
			added: 30, used: 33,
			wantOK: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cost, ok := c.g.Cost(c.added, c.used, c.durationMin)
			if ok != c.wantOK {
				t.Fatalf("expected ok=%v, got %v", c.wantOK, ok)
			}
			if ok && (cost-c.wantCost > 0.0001 || cost-c.wantCost < -0.0001) {
				t.Fatalf("expected cost %.4f, got %.4f", c.wantCost, cost)
			}
		})
	}
}

func TestFindGeofenceReturnsTheZoneNotJustTheName(t *testing.T) {
	geofences := []Geofence{
		{Name: "Home", Lat: 35.0, Lng: -85.0, RadiusM: 100, HasPricing: true, BillingType: "per_kwh", CostPerUnit: 0.125},
	}
	r := New(geofences, nil, false, "", "test")

	g, ok := r.FindGeofence(35.00035, -85.0)
	if !ok {
		t.Fatalf("expected to find the containing geofence")
	}
	if g.Name != "Home" || g.CostPerUnit != 0.125 {
		t.Fatalf("expected the full zone including its pricing, got %+v", g)
	}

	if _, ok := r.FindGeofence(36.0, -86.0); ok {
		t.Fatalf("expected no match far outside every zone")
	}
}

func TestResolveOutsideGeofenceRadiusDoesNotMatch(t *testing.T) {
	geofences := []Geofence{{Name: "Home", Lat: 35.0000, Lng: -85.0000, RadiusM: 10}}
	r := New(geofences, nil, false, "", "test")

	// ~50m away, outside the 10m radius.
	got := r.Resolve(context.Background(), 35.00035, -85.0000)
	if got != "" {
		t.Fatalf("expected no match outside the geofence radius, got %q", got)
	}
}

func TestResolveUsesCacheBeforeNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"name":"Should Not Be Used"}`))
	}))
	defer server.Close()

	cache := newMemCache()
	cache.m[[2]float64{35.0, -85.0}] = Place{Name: "Cached Place"}

	r := New(nil, cache, true, server.URL, "test")
	got := r.Resolve(context.Background(), 35.0, -85.0)
	if got != "Cached Place" {
		t.Fatalf("expected cached name, got %q", got)
	}
	if calls != 0 {
		t.Fatalf("expected no network call when the cache already has an entry, got %d calls", calls)
	}
}

func TestResolveDisabledWithNoGeofenceMatchReturnsEmpty(t *testing.T) {
	r := New(nil, nil, false, "", "test")
	got := r.Resolve(context.Background(), 35.0, -85.0)
	if got != "" {
		t.Fatalf("expected empty string when geocoding disabled and no geofence matched, got %q", got)
	}
}

func TestResolveFetchesAndCachesFromReverseGeocodeService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "test-agent" {
			t.Errorf("expected User-Agent 'test-agent', got %q", got)
		}
		w.Write([]byte(`{"name":"","address":{"amenity":"Publix Parking Lot"}}`))
	}))
	defer server.Close()

	cache := newMemCache()
	r := New(nil, cache, true, server.URL, "test-agent")

	got := r.Resolve(context.Background(), 35.12345, -85.54321)
	if got != "Publix Parking Lot" {
		t.Fatalf("expected 'Publix Parking Lot', got %q", got)
	}

	// Second call for the same (rounded) coordinate must come from the
	// cache this time, not hit the server again.
	cached, ok, err := cache.Lookup(roundCoord(35.12345), roundCoord(-85.54321))
	if err != nil || !ok || cached.Name != "Publix Parking Lot" {
		t.Fatalf("expected the result to have been cached, got name=%q ok=%v err=%v", cached.Name, ok, err)
	}
}

func TestReverseGeocodeAddressFallbackOrder(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"prefers POI name", `{"name":"Tesla Supercharger"}`, "Tesla Supercharger"},
		{"falls back to amenity", `{"address":{"amenity":"Coffee Shop"}}`, "Coffee Shop"},
		{"falls back to shop", `{"address":{"shop":"Grocery"}}`, "Grocery"},
		{"falls back to house number + road", `{"address":{"house_number":"123","road":"Main St"}}`, "123 Main St"},
		{"falls back to road alone", `{"address":{"road":"Main St"}}`, "Main St"},
		{"falls back to display_name", `{"display_name":"Somewhere, Nowhere County"}`, "Somewhere, Nowhere County"},
		{"empty response resolves to empty", `{}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(c.body))
			}))
			defer server.Close()

			r := New(nil, nil, true, server.URL, "test")
			got := r.Resolve(context.Background(), 1, 2)
			if got != c.want {
				t.Fatalf("expected %q, got %q", c.want, got)
			}
		})
	}
}

func TestResolveNominatimErrorReturnsEmptyNotPanic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":"Unable to geocode"}`))
	}))
	defer server.Close()

	r := New(nil, nil, true, server.URL, "test")
	got := r.Resolve(context.Background(), 1, 2)
	if got != "" {
		t.Fatalf("expected empty string on a Nominatim error response, got %q", got)
	}
}

// TestResolveCachesAddressComponents pins that the administrative parts
// of the Nominatim response are kept, not just the display name. They
// arrive in the same payload (we always send addressdetails=1) and were
// silently discarded, which made "# of cities/states/countries visited"
// - four panels of TeslaMate's Locations dashboard - impossible to
// answer from teslalog's data at all.
func TestResolveCachesAddressComponents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"Publix Parking Lot","address":{
			"road":"Gunbarrel Rd","town":"East Ridge","county":"Hamilton County",
			"state":"Tennessee","postcode":"37412","country":"United States"}}`))
	}))
	defer server.Close()

	cache := newMemCache()
	r := New(nil, cache, true, server.URL, "test")
	if got := r.Resolve(context.Background(), 35.12345, -85.54321); got != "Publix Parking Lot" {
		t.Fatalf("expected the display name to be unchanged, got %q", got)
	}

	place, ok, err := cache.Lookup(roundCoord(35.12345), roundCoord(-85.54321))
	if err != nil || !ok {
		t.Fatalf("expected a cached place, got ok=%v err=%v", ok, err)
	}
	for _, field := range []struct{ label, got, want string }{
		{"road", place.Road, "Gunbarrel Rd"},
		// Nominatim files a US suburb under "town", never "city", so
		// reading only the "city" key would have lost most settlements.
		{"city", place.City, "East Ridge"},
		{"county", place.County, "Hamilton County"},
		{"state", place.State, "Tennessee"},
		{"postcode", place.Postcode, "37412"},
		{"country", place.Country, "United States"},
	} {
		if field.got != field.want {
			t.Errorf("%s: got %q, want %q", field.label, field.got, field.want)
		}
	}
}

// TestCityPrefersMostSpecificSettlementKey pins the key order. A place
// that reports both a village and a municipality is in the village; the
// municipality is the larger administrative unit around it.
func TestCityPrefersMostSpecificSettlementKey(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"address":{"city":"Chattanooga","town":"X","village":"Y","municipality":"Z"}}`, "Chattanooga"},
		{`{"address":{"town":"East Ridge","village":"Y","municipality":"Z"}}`, "East Ridge"},
		{`{"address":{"village":"Soddy-Daisy","municipality":"Z"}}`, "Soddy-Daisy"},
		{`{"address":{"municipality":"Hamilton"}}`, "Hamilton"},
		{`{"address":{"road":"Nowhere Rd"}}`, ""},
	}
	for _, tc := range cases {
		var nr nominatimResponse
		if err := json.Unmarshal([]byte(tc.body), &nr); err != nil {
			t.Fatalf("unmarshal %s: %v", tc.body, err)
		}
		if got := nr.city(); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.body, got, tc.want)
		}
	}
}
