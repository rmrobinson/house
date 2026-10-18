package main

import (
	"embed"
	"html/template"
	"strings"
)

//go:embed templates
var templatesFS embed.FS

// templateFuncs is available to every template here.
var templateFuncs = template.FuncMap{"domid": domID}

// domID makes a device ID safe to embed in an element id and in the
// "#<id>" selectors htmx builds from it (hx-target, and the id match for
// out-of-band swaps - htmx 2 does not CSS-escape them). Device IDs are
// bridge-chosen and routinely contain characters that are not valid in a
// selector: the monop-amp bridge's "<bridge-uuid>:2", an IP, a MAC, a space.
// Anything outside [A-Za-z0-9-] becomes "_" plus two hex digits - including
// "_" itself, so the mapping is injective and two IDs can't collide.
func domID(id string) string {
	const hexdigits = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
			b.WriteByte(hexdigits[c>>4])
			b.WriteByte(hexdigits[c&0xf])
		}
	}
	return b.String()
}

// pages combines templates/layout.html with each page's own content
// template - html/template has no "extends", and "content" can only be
// defined once per *template.Template, so each page gets its own instance
// (same approach as adminui's templates.go).
var pages = map[string]*template.Template{
	"buildings": mustParsePage("templates/buildings.html"),
	"overview":  mustParsePage("templates/overview.html", "templates/partials/overview.html"),
	"building": mustParsePage("templates/building.html",
		"templates/partials/floor_panel.html", "templates/partials/room_detail.html", "templates/partials/device_row.html"),
}

// fragments holds every partial, for htmx swaps and the SSE relay (sse.go).
var fragments = template.Must(template.New("").Funcs(templateFuncs).ParseFS(templatesFS, "templates/partials/*.html"))

func mustParsePage(files ...string) *template.Template {
	all := append([]string{"templates/layout.html"}, files...)
	return template.Must(template.New("layout.html").Funcs(templateFuncs).ParseFS(templatesFS, all...))
}
