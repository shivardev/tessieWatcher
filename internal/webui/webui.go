// Package webui embeds the compiled browser viewer (frontend/dist) into
// the teslalog binary and serves it from the portal.
//
// Why serve it from the Pi at all, when the same build also publishes to
// GitHub Pages: the viewer's live connection reads /api/meta and
// /download from a running teslalog, and a page served over HTTPS cannot
// fetch a plain-HTTP LAN address - browsers block it before any request
// is made. GitHub Pages is HTTPS-only, so the hosted copy can only ever
// open a file the user downloaded by hand. Served from the portal, the
// viewer is same-origin with the API it wants to read, so it can connect
// to itself with nothing to configure and no address to type.
//
// The assets are committed, not built on demand, so `go build` works
// from a clean checkout and a release binary is reproducible from a tag.
// They are refreshed by deploy/cross-build.sh, which runs the frontend
// build first. See frontend/README.md.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// dist is the Vite build output. "all:" is required because the build
// emits an assets/ directory and embed otherwise skips paths whose name
// begins with "_" or "." - Vite has used both in the past.
//
//go:embed all:dist
var dist embed.FS

// Assets returns the compiled viewer as a filesystem rooted at the
// directory holding index.html.
func Assets() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}

// Available reports whether a real viewer was embedded. A checkout whose
// assets were never built carries only the placeholder, and the portal
// says so plainly rather than serving a broken page.
func Available() bool {
	entries, err := dist.ReadDir("dist/assets")
	return err == nil && len(entries) > 0
}

// Handler serves the viewer under prefix (e.g. "/app"). Unknown paths
// fall back to index.html: the viewer keeps its navigation in React
// state rather than in the URL today, but a deep link typed by hand
// should land on the app rather than on a 404.
func Handler(prefix string) (http.Handler, error) {
	assets, err := Assets()
	if err != nil {
		return nil, err
	}
	files := http.FileServer(http.FS(assets))
	stripped := http.StripPrefix(strings.TrimSuffix(prefix, "/"), files)

	base := strings.TrimSuffix(prefix, "/")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, base), "/")

		// http.FileServer answers an explicit "index.html" with a 301 to
		// "./" to canonicalise the URL. Harmless in a browser but a
		// surprise to anything else, so serve it directly instead.
		if path == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
			serveIndex(w, r, assets)
			return
		}

		if path != "" && exists(assets, path) {
			// Asset filenames carry a content hash, so they can be cached
			// forever. index.html cannot: it names the current hashes, and
			// a cached copy would ask an upgraded binary for assets it no
			// longer has.
			if strings.HasPrefix(path, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			stripped.ServeHTTP(w, r)
			return
		}

		// A missing asset is a real 404. Answering it with index.html
		// would hand the browser HTML where it asked for a script, and
		// the resulting MIME error hides the actual problem.
		if strings.HasPrefix(path, "assets/") {
			http.NotFound(w, r)
			return
		}

		// Anything else falls back to the app: the viewer keeps its
		// navigation in React state rather than in the URL today, but a
		// deep link typed by hand should land on the app, not a 404.
		w.Header().Set("Cache-Control", "no-cache")
		serveIndex(w, r, assets)
	}), nil
}

func exists(assets fs.FS, path string) bool {
	file, err := assets.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	return err == nil && !info.IsDir()
}

func serveIndex(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	body, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		http.Error(w, "viewer assets missing from this build", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader(string(body)))
}
