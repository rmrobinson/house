package main

import (
	"net/http"
	"strings"
	"time"
)

// mediaReadHeaderTimeout bounds how long a client may take to send its
// request headers, so a stalled connection can't be held open forever. It
// deliberately doesn't bound the response: a speaker streams an hour-long
// file at its own pace.
const mediaReadHeaderTimeout = 10 * time.Second

// newMediaServer returns the HTTP server for serving dir, with timeouts set.
func newMediaServer(dir string) *http.Server {
	return &http.Server{
		Handler:           newMediaHandler(dir),
		ReadHeaderTimeout: mediaReadHeaderTimeout,
	}
}

// newMediaHandler serves the files under dir read-only, for use as a
// PlayMedia content_url. A Cast device fetches that URL itself, so serving
// the file from the LAN keeps looping playback independent of the internet
// once the receiver app has been launched.
//
// http.FileServer already honours Range requests, which receivers use to
// buffer and seek. Directory listings are turned off (a request for a
// directory, a trailing slash, is a 404 rather than an index of dir), and so
// is anything whose path has a dot-prefixed element, which keeps hidden files
// (.env, .git) out of reach if dir is ever pointed somewhere broader than a
// folder of media. Symlinks inside dir are followed, deliberately: pointing a
// name in dir at a file kept elsewhere is the easy way to publish one file.
func newMediaHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/") || hasDotElement(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		fs.ServeHTTP(w, r)
	})
}

// hasDotElement reports whether any element of the slash-separated path starts
// with a dot.
func hasDotElement(path string) bool {
	for _, el := range strings.Split(path, "/") {
		if strings.HasPrefix(el, ".") {
			return true
		}
	}
	return false
}
