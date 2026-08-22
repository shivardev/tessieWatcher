// Package geocode turns a (lat, lng) pair into a human-readable place
// name, the same job TeslaMate's geofences/locations tables do -
// teslalog's own simpler take on it: check user-named zones first (free,
// no network - config.toml's [[geofence]] entries), then a persistent
// cache, then (only if explicitly enabled) a reverse-geocoding HTTP
// lookup against an OSM Nominatim-compatible service.
//
// Resolve never errors: if nothing can be determined (geocoding
// disabled, no geofence matched, or the lookup failed), it returns "" -
// callers should treat that as "show the raw coordinates instead," not
// as a failure needing a retry.
package geocode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Geofence is a user-named circular zone (config.toml's [[geofence]]),
// TeslaMate's own primary way of naming home/work/etc. without ever
// touching a geocoding service.
type Geofence struct {
	Name    string
	Lat     float64
	Lng     float64
	RadiusM float64
}

// Cache persists resolved (roundCoord(lat), roundCoord(lng)) -> name
// lookups so the same spot is never reverse-geocoded twice - both to
// respect the geocoding service's rate limit and to avoid unnecessary
// network calls on every drive. *storage.Store satisfies this directly.
type Cache interface {
	Lookup(latKey, lngKey float64) (name string, ok bool, err error)
	Save(latKey, lngKey float64, name string) error
}

// Resolver turns coordinates into place names. Zero value is not usable
// for reverse-geocoding (use New); a Resolver with Enabled=false and no
// Geofences is valid and just always returns "".
type Resolver struct {
	geofences []Geofence
	cache     Cache
	enabled   bool
	baseURL   string
	userAgent string
	client    *http.Client

	mu       sync.Mutex
	lastCall time.Time
}

// New constructs a Resolver. cache may be nil (no persistence - every
// lookup hits the network, if enabled). baseURL/userAgent are only used
// when enabled is true.
func New(geofences []Geofence, cache Cache, enabled bool, baseURL, userAgent string) *Resolver {
	return &Resolver{
		geofences: geofences,
		cache:     cache,
		enabled:   enabled,
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		userAgent: userAgent,
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Resolve returns a place name for (lat, lng), checking geofences, then
// the cache, then (if enabled) a live reverse-geocoding lookup, in that
// cheapest-first order. Returns "" if none of those produced a name.
func (r *Resolver) Resolve(ctx context.Context, lat, lng float64) string {
	// Pick the NEAREST containing geofence, not merely the first one
	// listed - matching TeslaMate's own find_geofence, which orders
	// candidates by distance and takes the closest (verified directly
	// against lib/teslamate/locations.ex). This matters whenever zones
	// overlap, e.g. a small "Home garage" inside a larger "Home": the
	// more specific one wins regardless of config order, instead of
	// the answer depending on how the file happens to be arranged.
	bestName := ""
	bestDist := math.Inf(1)
	for _, g := range r.geofences {
		d := haversineMeters(lat, lng, g.Lat, g.Lng)
		if d <= g.RadiusM && d < bestDist {
			bestName, bestDist = g.Name, d
		}
	}
	if bestName != "" {
		return bestName
	}

	latKey, lngKey := roundCoord(lat), roundCoord(lng)
	if r.cache != nil {
		if name, ok, err := r.cache.Lookup(latKey, lngKey); err == nil && ok {
			return name
		}
	}

	if !r.enabled {
		return ""
	}

	name, err := r.reverseGeocode(ctx, lat, lng)
	if err != nil || name == "" {
		return ""
	}
	if r.cache != nil {
		_ = r.cache.Save(latKey, lngKey, name)
	}
	return name
}

// roundCoord buckets a coordinate to ~11m precision (4 decimal degrees)
// for cache-key purposes - fine-grained enough to distinguish adjacent
// businesses/parking lots, coarse enough that GPS noise between visits
// to the "same" spot still hits the same cache entry.
func roundCoord(v float64) float64 {
	return math.Round(v*10000) / 10000
}

func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusM = 6371000.0
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat := toRad(lat2 - lat1)
	dLng := toRad(lng2 - lng1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusM * c
}

type nominatimResponse struct {
	DisplayName string `json:"display_name"`
	Name        string `json:"name"`
	Address     struct {
		Amenity     string `json:"amenity"`
		Shop        string `json:"shop"`
		Road        string `json:"road"`
		HouseNumber string `json:"house_number"`
	} `json:"address"`
	Error string `json:"error"`
}

// reverseGeocode queries an OSM Nominatim-compatible /reverse endpoint.
// Self-throttles to at most one request/second regardless of caller
// concurrency, per Nominatim's usage policy
// (https://operations.osmfoundation.org/policies/nominatim/) - teslalog
// only ever calls this at most twice per drive and once per charge, so
// this practically never actually waits, but it's a hard guarantee
// rather than a hope.
func (r *Resolver) reverseGeocode(ctx context.Context, lat, lng float64) (string, error) {
	r.mu.Lock()
	if wait := time.Second - time.Since(r.lastCall); wait > 0 {
		time.Sleep(wait)
	}
	r.lastCall = time.Now()
	r.mu.Unlock()

	url := fmt.Sprintf("%s/reverse?format=jsonv2&lat=%f&lon=%f&zoom=18&addressdetails=1", r.baseURL, lat, lng)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", r.userAgent)

	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("reverse geocode: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var nr nominatimResponse
	if err := json.Unmarshal(body, &nr); err != nil {
		return "", fmt.Errorf("decode reverse geocode response: %w", err)
	}
	if nr.Error != "" {
		return "", fmt.Errorf("reverse geocode: %s", nr.Error)
	}

	// Prefer a business/POI name over a bare address, matching TeslaMate's
	// own address display (e.g. "Publix Parking Lot", not just a street
	// number).
	switch {
	case nr.Name != "":
		return nr.Name, nil
	case nr.Address.Amenity != "":
		return nr.Address.Amenity, nil
	case nr.Address.Shop != "":
		return nr.Address.Shop, nil
	case nr.Address.Road != "" && nr.Address.HouseNumber != "":
		return nr.Address.HouseNumber + " " + nr.Address.Road, nil
	case nr.Address.Road != "":
		return nr.Address.Road, nil
	case nr.DisplayName != "":
		return nr.DisplayName, nil
	default:
		return "", nil
	}
}
