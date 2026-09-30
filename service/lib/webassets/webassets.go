// Package webassets embeds the vendored front-end libraries every htmx UI in
// this repo (adminui, viewerui) loads, so one copy lives in the tree instead
// of one per UI. They're served from the binary itself - no CDN, so a UI has
// no runtime dependency on outside network access.
package webassets

import (
	"embed"
	"net/http"
)

//go:embed static
var fs embed.FS

// Register mounts htmx and its SSE extension at /static/htmx.min.js and
// /static/htmx-sse.js on mux. A UI's own "GET /static/" handler can coexist
// with these - ServeMux prefers the more specific exact-path pattern.
func Register(mux *http.ServeMux) {
	h := http.FileServerFS(fs)
	mux.Handle("GET /static/htmx.min.js", h)
	mux.Handle("GET /static/htmx-sse.js", h)
}
