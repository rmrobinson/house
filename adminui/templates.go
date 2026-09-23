package main

import (
	"embed"
	"html/template"
)

//go:embed static
var staticFS embed.FS

//go:embed templates
var templatesFS embed.FS

// pages combines templates/layout.html with each page's own content
// template. html/template has no "extends" mechanism - a template named
// "content" can only be defined once per *template.Template - so each page
// gets its own combined instance rather than sharing one template set.
var pages = map[string]*template.Template{
	"buildings": mustParsePage("templates/buildings.html"),
	"building":  mustParsePage("templates/building.html"),
	"floor":     mustParsePage("templates/floor.html"),
	// room/devices embed the device_info partial directly (via
	// {{template "device_info" .}}) for their own device table rows. The SSE
	// relay (sse.go) renders the same file's device_info_oob block instead -
	// same markup, but with hx-swap-oob set - since a live push has to target
	// an id already on the page rather than land as part of a larger swap;
	// see templates/partials/device_info.html for why the two can't share a
	// single block.
	"room":    mustParsePage("templates/room.html", "templates/partials/device_info.html"),
	"devices": mustParsePage("templates/devices.html", "templates/partials/device_info.html"),
}

// fragments are partials rendered standalone (no layout) - pickers, and
// device_info again for the SSE relay (sse.go)'s device_info_oob block,
// which has no page of its own to attach to.
var fragments = map[string]*template.Template{
	"device_picker": template.Must(template.ParseFS(templatesFS, "templates/partials/device_picker.html")),
	"room_picker":   template.Must(template.ParseFS(templatesFS, "templates/partials/room_picker.html")),
	"device_info":   template.Must(template.ParseFS(templatesFS, "templates/partials/device_info.html")),
}

func mustParsePage(files ...string) *template.Template {
	all := append([]string{"templates/layout.html"}, files...)
	return template.Must(template.ParseFS(templatesFS, all...))
}
