package main

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"time"
)

// pwaFS holds what lets a phone install viewerui as an app: the web manifest,
// the service worker, the offline page and the icons. See pwa/sw.js for why
// the worker caches almost nothing.
//
//go:embed pwa
var pwaFS embed.FS

// registerPWA mounts the install assets. The manifest and worker live at the
// site root (a worker's scope is limited to its own path, and it has to cover
// every page); the icons go under /static/ so protect() leaves them cacheable.
func (s *Server) registerPWA(mux *http.ServeMux) {
	mux.HandleFunc("GET /manifest.webmanifest", s.servePWA("pwa/manifest.webmanifest", "application/manifest+json", "no-cache"))
	// no-cache, not no-store: browsers revalidate the worker on every
	// navigation anyway, and this lets a new version roll out immediately.
	mux.HandleFunc("GET /sw.js", s.servePWA("pwa/sw.js", "text/javascript; charset=utf-8", "no-cache"))
	mux.HandleFunc("GET /offline", s.servePWA("pwa/offline.html", "text/html; charset=utf-8", "no-cache"))
	mux.HandleFunc("GET /static/icons/{name}", func(w http.ResponseWriter, r *http.Request) {
		// PathValue is a single segment, and ReadFile rejects anything that
		// isn't a valid embedded path, so this can't escape pwa/icons.
		b, err := fs.ReadFile(pwaFS, path.Join("pwa/icons", r.PathValue("name")))
		if err != nil || path.Ext(r.PathValue("name")) != ".png" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	})
}

func (s *Server) servePWA(name, contentType, cacheControl string) http.HandlerFunc {
	b, err := fs.ReadFile(pwaFS, name)
	if err != nil {
		panic(err) // embedded at build time; a miss is a programming error
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", cacheControl)
		w.Write(b)
	}
}
