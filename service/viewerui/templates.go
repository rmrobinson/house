package main

import (
	"embed"
	"html/template"
)

//go:embed templates
var templatesFS embed.FS

// pages combines templates/layout.html with each page's own content
// template - html/template has no "extends", and "content" can only be
// defined once per *template.Template, so each page gets its own instance
// (same approach as adminui's templates.go).
var pages = map[string]*template.Template{
	"buildings": mustParsePage("templates/buildings.html"),
	"building": mustParsePage("templates/building.html",
		"templates/partials/floor_panel.html", "templates/partials/room_detail.html", "templates/partials/device_row.html"),
}

// fragments holds every partial, for htmx swaps and the SSE relay (sse.go).
var fragments = template.Must(template.ParseFS(templatesFS, "templates/partials/*.html"))

func mustParsePage(files ...string) *template.Template {
	all := append([]string{"templates/layout.html"}, files...)
	return template.Must(template.New("layout.html").ParseFS(templatesFS, all...))
}
