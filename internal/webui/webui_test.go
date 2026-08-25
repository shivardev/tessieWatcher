package webui

import (
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlerServesTheViewerAtItsPrefix pins the arrangement the live
// connection depends on. The viewer polls /api/meta and /download from
// whatever origin served it; a page served over HTTPS cannot fetch a
// plain-HTTP LAN address, so a same-origin copy on the portal is the
// only configuration where the live connection can work at all.
func TestHandlerServesTheViewerAtItsPrefix(t *testing.T) {
	if !Available() {
		t.Skip("frontend assets were not built into this checkout")
	}
	handler, err := Handler("/app")
	if err != nil {
		t.Fatalf("build handler: %v", err)
	}

	for _, path := range []string{"/app", "/app/", "/app/index.html"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Errorf("%s: expected 200, got %d", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<div id=\"root\"") {
			t.Errorf("%s: expected the viewer's HTML shell", path)
		}
	}
}

// TestHandlerFallsBackToIndex covers a deep link typed by hand. The
// viewer keeps its navigation in React state rather than in the URL
// today, so any sub-path should land on the app rather than a 404 - and
// should keep doing so if URL-based routing is ever added.
func TestHandlerFallsBackToIndex(t *testing.T) {
	if !Available() {
		t.Skip("frontend assets were not built into this checkout")
	}
	handler, _ := Handler("/app")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/app/drives/42", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<div id=\"root\"") {
		t.Fatalf("expected the index fallback, got %d", rec.Code)
	}
}

// TestAssetsAreCachedButIndexIsNot pins the pairing that makes an
// in-place upgrade safe. Asset filenames carry a content hash, so they
// can be cached forever; index.html names the current hashes, and a
// cached copy would ask an updated binary for assets it no longer has.
func TestAssetsAreCachedButIndexIsNot(t *testing.T) {
	if !Available() {
		t.Skip("frontend assets were not built into this checkout")
	}
	handler, _ := Handler("/app")

	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest("GET", "/app/", nil))
	if got := index.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("index.html must not be cached, got %q", got)
	}

	assets, err := Assets()
	if err != nil {
		t.Fatalf("assets: %v", err)
	}
	entries, err := fs.ReadDir(assets, "assets")
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected built assets, got %d entries (err %v)", len(entries), err)
	}

	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, httptest.NewRequest("GET", "/app/assets/"+entries[0].Name(), nil))
	if asset.Code != 200 {
		t.Fatalf("expected the asset to be served, got %d", asset.Code)
	}
	if got := asset.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("hashed assets must be cached immutably, got %q", got)
	}
}

// TestMissingAssetIs404 rather than the index fallback: answering a
// request for a script with HTML produces a MIME error that hides the
// real problem, and it must not be cached as though it were an asset.
func TestMissingAssetIs404(t *testing.T) {
	if !Available() {
		t.Skip("frontend assets were not built into this checkout")
	}
	handler, _ := Handler("/app")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/app/assets/gone-DEADBEEF.js", nil))
	if rec.Code != 404 {
		t.Errorf("expected 404 for a missing asset, got %d", rec.Code)
	}
	if strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("a 404 must not be cached immutably")
	}
}
