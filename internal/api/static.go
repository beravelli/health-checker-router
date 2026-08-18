package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// staticHandler serves the built React bundle from dir.
//
// The container image already ships the bundle — the Dockerfile copies
// web/dist to /app/web — so without this the deployed pod answers /api/* but
// returns 404 for the console itself.
//
// Two behaviours the standard http.FileServer does not give us:
//
//   - SPA fallback. The console is client-routed, so a deep link like
//     /rollouts is not a file on disk. Anything that is not a real file and
//     not an API path returns index.html and lets the router sort it out.
//   - No directory listings. A request for a directory serves its index.html
//     or 404s, rather than exposing a browsable tree.
//
// If dir does not exist the handler reports that once at startup and serves
// 404s, which is the correct behaviour for an API-only deployment (running the
// frontend separately with `npm run dev`) rather than a reason to fail boot.
func staticHandler(dir string) http.Handler {
	index := filepath.Join(dir, "index.html")
	available := fileExists(index)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !available {
			http.NotFound(w, r)
			return
		}

		// Never let a static path shadow the API surface.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}

		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		// filepath.Clean leaves a leading .. in place for traversal attempts;
		// reject rather than resolve them.
		if clean == "." || clean == "/" || strings.HasPrefix(clean, "..") {
			serveIndex(w, r, index)
			return
		}

		target := filepath.Join(dir, clean)
		if !fileExists(target) {
			// Unknown path: hand it to the client router.
			serveIndex(w, r, index)
			return
		}

		// Hashed asset filenames are content-addressed and safe to cache hard.
		// index.html must not be, or a deploy never reaches the browser.
		if strings.HasPrefix(clean, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.ServeFile(w, r, target)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, index string) {
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, index)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
