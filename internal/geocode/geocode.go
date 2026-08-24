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

	// Pricing for charges that happen inside this zone. Real-world
	// electricity rates differ per location - home vs a paid apartment
	// charger vs a free one at a store - so a single global rate can't
	// produce correct costs for anyone with more than one regular
	// charging spot. Matches TeslaMate's own per-geofence pricing
	// (geofences.cost_per_unit/billing_type/session_fee, verified
	// against lib/teslamate/log.ex's put_cost).
	//
	// BillingType is "per_kwh" (default) or "per_minute". CostPerUnit
	// is currency per kWh or per minute accordingly; zero is a
	// legitimate value meaning free. SessionFee is a flat charge added
	// once per session on top.
	//
	// HasPricing distinguishes "this zone charges nothing" (a free
	// charger - cost 0) from "no pricing configured for this zone"
	// (cost unknown, left NULL), which a zero CostPerUnit alone
	// can't express.
	HasPricing  bool
	BillingType string
	CostPerUnit float64
	SessionFee  float64
}

// Cost computes what a charging session inside this geofence cost,
// following TeslaMate's put_cost exactly (verified against its
// source): per-kWh billing charges for the greater of energy added
// and energy drawn from the wall, per-minute billing charges for
// elapsed time, and either way a flat session fee is added on top.
// Returns ok=false if this zone has no pricing configured, so the
// caller can leave the cost unknown rather than recording a
// misleading zero.
func (g Geofence) Cost(energyAddedKwh, energyUsedKwh, durationMin float64) (cost float64, ok bool) {
	if !g.HasPricing {
		return 0, false
	}
	switch g.BillingType {
	case "per_minute":
		return g.SessionFee + durationMin*g.CostPerUnit, true
	default: // "per_kwh"
		// TeslaMate bills the greater of the two: energy drawn from
		// the wall is what a supplier meters, but it isn't always
		// derivable, in which case energy added is the best available
		// stand-in.
		kwh := energyAddedKwh
		if energyUsedKwh > kwh {
			kwh = energyUsedKwh
		}
		return g.SessionFee + kwh*g.CostPerUnit, true
	}
}

// Cache persists resolved (roundCoord(lat), roundCoord(lng)) -> name
// lookups so the same spot is never reverse-geocoded twice - both to
// respect the geocoding service's rate limit and to avoid unnecessary
// network calls on every drive. *storage.Store satisfies this directly.
type Cache interface {
	Lookup(latKey, lngKey float64) (place Place, ok bool, err error)
	Save(latKey, lngKey float64, place Place) error
}

// Place is a resolved address. Name is the short display label used
// everywhere a location is shown; the rest are the administrative
// components Nominatim already returns alongside it (we pass
// addressdetails=1 either way). They are stored so the viewer can
// answer "how many cities/states/countries have I driven in", which
// TeslaMate answers from its own addresses table and which a single
// display string cannot support. Any of them may be empty - a rural
// road has no city, and a geofence hit never reaches Nominatim at all.
type Place struct {
	Name     string
	Road     string
	City     string
	County   string
	State    string
	Postcode string
	Country  string
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

// FindGeofence returns the configured geofence containing (lat, lng),
// or ok=false if none does. Exposed separately from Resolve because
// callers sometimes need more than the name - notably the charging
// cost, which is per-geofence (see Geofence.Cost).
//
// Picks the NEAREST containing zone, not merely the first one listed -
// matching TeslaMate's own find_geofence, which orders candidates by
// distance and takes the closest (verified directly against
// lib/teslamate/locations.ex). This matters whenever zones overlap,
// e.g. a small "Home garage" inside a larger "Home": the more specific
// one wins regardless of config order, instead of the answer depending
// on how the file happens to be arranged.
func (r *Resolver) FindGeofence(lat, lng float64) (Geofence, bool) {
	var best Geofence
	found := false
	bestDist := math.Inf(1)
	for _, g := range r.geofences {
		d := haversineMeters(lat, lng, g.Lat, g.Lng)
		if d <= g.RadiusM && d < bestDist {
			best, bestDist, found = g, d, true
		}
	}
	return best, found
}

// Resolve returns a place name for (lat, lng), checking geofences, then
// the cache, then (if enabled) a live reverse-geocoding lookup, in that
// cheapest-first order. Returns "" if none of those produced a name.
func (r *Resolver) Resolve(ctx context.Context, lat, lng float64) string {
	if g, ok := r.FindGeofence(lat, lng); ok {
		return g.Name
	}

	latKey, lngKey := roundCoord(lat), roundCoord(lng)
	if r.cache != nil {
		if place, ok, err := r.cache.Lookup(latKey, lngKey); err == nil && ok {
			return place.Name
		}
	}

	if !r.enabled {
		return ""
	}

	place, err := r.reverseGeocode(ctx, lat, lng)
	if err != nil || place.Name == "" {
		return ""
	}
	if r.cache != nil {
		_ = r.cache.Save(latKey, lngKey, place)
	}
	return place.Name
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
		// Nominatim reports the settlement under whichever of these
		// keys fits the place's administrative type, so all four have
		// to be read to get a city name reliably: a US suburb is often
		// only a "town" or "village", never a "city".
		City         string `json:"city"`
		Town         string `json:"town"`
		Village      string `json:"village"`
		Municipality string `json:"municipality"`
		County       string `json:"county"`
		State        string `json:"state"`
		Postcode     string `json:"postcode"`
		Country      string `json:"country"`
		CountryCode  string `json:"country_code"`
	} `json:"address"`
	Error string `json:"error"`
}

// city returns the settlement name from whichever key Nominatim used.
func (nr *nominatimResponse) city() string {
	for _, candidate := range []string{
		nr.Address.City, nr.Address.Town, nr.Address.Village, nr.Address.Municipality,
	} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

// reverseGeocode queries an OSM Nominatim-compatible /reverse endpoint.
// Self-throttles to at most one request/second regardless of caller
// concurrency, per Nominatim's usage policy
// (https://operations.osmfoundation.org/policies/nominatim/) - teslalog
// only ever calls this at most twice per drive and once per charge, so
// this practically never actually waits, but it's a hard guarantee
// rather than a hope.
func (r *Resolver) reverseGeocode(ctx context.Context, lat, lng float64) (Place, error) {
	r.mu.Lock()
	if wait := time.Second - time.Since(r.lastCall); wait > 0 {
		time.Sleep(wait)
	}
	r.lastCall = time.Now()
	r.mu.Unlock()

	url := fmt.Sprintf("%s/reverse?format=jsonv2&lat=%f&lon=%f&zoom=18&addressdetails=1", r.baseURL, lat, lng)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Place{}, err
	}
	req.Header.Set("User-Agent", r.userAgent)

	resp, err := r.client.Do(req)
	if err != nil {
		return Place{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Place{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Place{}, fmt.Errorf("reverse geocode: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var nr nominatimResponse
	if err := json.Unmarshal(body, &nr); err != nil {
		return Place{}, fmt.Errorf("decode reverse geocode response: %w", err)
	}
	if nr.Error != "" {
		return Place{}, fmt.Errorf("reverse geocode: %s", nr.Error)
	}

	place := Place{
		Road:     nr.Address.Road,
		City:     nr.city(),
		County:   nr.Address.County,
		State:    nr.Address.State,
		Postcode: nr.Address.Postcode,
		Country:  nr.Address.Country,
	}

	// Prefer a business/POI name over a bare address, matching TeslaMate's
	// own address display (e.g. "Publix Parking Lot", not just a street
	// number).
	switch {
	case nr.Name != "":
		place.Name = nr.Name
	case nr.Address.Amenity != "":
		place.Name = nr.Address.Amenity
	case nr.Address.Shop != "":
		place.Name = nr.Address.Shop
	case nr.Address.Road != "" && nr.Address.HouseNumber != "":
		place.Name = nr.Address.HouseNumber + " " + nr.Address.Road
	case nr.Address.Road != "":
		place.Name = nr.Address.Road
	case nr.DisplayName != "":
		place.Name = nr.DisplayName
	}
	return place, nil
}

// RefreshPlace re-resolves a cached coordinate from the geocoding
// service and overwrites its cache row, bypassing the cache read.
// Used to fill in address components on rows that were cached before
// teslalog stored them - the display name is already correct, so
// Resolve would short-circuit and they would stay incomplete forever.
// Reports false if geocoding is disabled or the lookup failed.
func (r *Resolver) RefreshPlace(ctx context.Context, latKey, lngKey float64) (Place, bool) {
	if !r.enabled || r.cache == nil {
		return Place{}, false
	}
	place, err := r.reverseGeocode(ctx, latKey, lngKey)
	if err != nil || place.Name == "" {
		return Place{}, false
	}
	if err := r.cache.Save(latKey, lngKey, place); err != nil {
		return Place{}, false
	}
	return place, true
}
