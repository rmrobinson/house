// Package htmxutil holds small, dependency-free helpers shared by every
// htmx-driven admin UI in this repo (adminui, service/policy's own UI):
// collapsing a rendered fragment to one line for SSE, and writing the
// shared #flash banner's out-of-band swap.
package htmxutil

import (
	"fmt"
	"html/template"
	"net/http"
)

// OneLine collapses s to a single line - the SSE wire format terminates a
// data field at the first newline, so a multi-line HTML fragment must be
// sent as consecutive "data: " lines instead of one. html/template's output
// is compact enough that stripping newlines outright is simpler than
// splitting into multiple "data:" lines, and doesn't change the rendered
// HTML.
func OneLine(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

// WriteFlash writes the shared #flash banner's out-of-band swap: every
// htmx-driven admin UI in this repo uses the same "#flash"/hx-swap-oob
// convention for surfacing an action's result. msg may be empty to clear
// the banner without showing a new message.
func WriteFlash(w http.ResponseWriter, msg string, isError bool) {
	class := "flash"
	if isError {
		class = "flash flash-error"
	}
	fmt.Fprintf(w, `<div id="flash" hx-swap-oob="true" class="%s">%s</div>`, class, template.HTMLEscapeString(msg))
}
