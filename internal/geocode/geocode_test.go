package geocode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// memCache is a trivial in-memory Cache for tests.
type memCache struct {
	m map[[2]float64]string
}

func newMemCache() *memCache { return &memCache{m: map[[2]float64]string{}} }

func (c *memCache) Lookup(latKey, lngKey float64) (string, bool, error) {
	name, ok := c.m[[2]float64{latKey, lngKey}]
	return name, ok, nil
}

func (c *memCache) Save(latKey, lngKey float64, name string) error {
	c.m[[2]float64{latKey, lngKey}] = name
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
	cache.m[[2]float64{35.0, -85.0}] = "Cached Place"

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
	name, ok, err := cache.Lookup(roundCoord(35.12345), roundCoord(-85.54321))
	if err != nil || !ok || name != "Publix Parking Lot" {
		t.Fatalf("expected the result to have been cached, got name=%q ok=%v err=%v", name, ok, err)
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
