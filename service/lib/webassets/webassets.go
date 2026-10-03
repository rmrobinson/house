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

// Register mounts htmx and its SSE extension at basePath+/static/htmx.min.js
// and basePath+/static/htmx-sse.js on mux. A UI's own "GET <basePath>/static/"
// handler can coexist with these - ServeMux prefers the more specific
// exact-path pattern. basePath is "" for a UI serving at its site root
// (every caller as of this writing); a UI running behind a reverse proxy
// that forwards a path prefix without stripping it (see adminui's
// adminui.base_path) passes that same prefix here so these two exact paths
// match what the proxy actually forwards.
func Register(mux *http.ServeMux, basePath string) {
	// http.FileServerFS looks a request up by its raw URL path, and fs's
	// embedded tree only knows paths like "static/htmx.min.js" - no base
	// path prefix in it - so basePath has to be stripped back off before
	// the lookup, not just matched going in.
	h := http.StripPrefix(basePath, http.FileServerFS(fs))
	mux.Handle("GET "+basePath+"/static/htmx.min.js", h)
	mux.Handle("GET "+basePath+"/static/htmx-sse.js", h)
}
