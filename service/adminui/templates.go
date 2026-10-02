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
	// policies/policy_detail/policy_editor need policyTemplateFuncs (see
	// policy_view.go) for formatTime/duration/conditionPretty/
	// onConditionFalseStr - ported from service/policy/http.go, which
	// registered the same funcs on its own now-removed template set.
	"policies":             mustParsePageFuncs(policyTemplateFuncs, "templates/policies.html", "templates/partials/policy_status.html"),
	"policy_detail":        mustParsePageFuncs(policyTemplateFuncs, "templates/policy_detail.html", "templates/partials/policy_status.html", "templates/partials/log_row.html"),
	"policy_editor":        mustParsePageFuncs(policyTemplateFuncs, "templates/policy_editor.html"),
	"logs":                 mustParsePageFuncs(policyTemplateFuncs, "templates/logs.html", "templates/partials/log_row.html"),
	"policy_delete_failed": mustParsePage("templates/policy_delete_failed.html"),
	// Shown by requirePolicy (app.go) in place of any /policies or /logs
	// route when adminui.policy_addr isn't configured.
	"policy_unavailable": mustParsePage("templates/policy_unavailable.html"),
	// Edits adminui.house_addr/bridge_facade_addr/policy_addr live - see
	// settings.go's app.rebuild.
	"settings": mustParsePage("templates/settings.html"),
}

// fragments are partials rendered standalone (no layout) - pickers,
// device_info again for the SSE relay (sse.go)'s device_info_oob block
// (which has no page of its own to attach to), and the same for
// policy_status/log_row's OOB blocks.
var fragments = map[string]*template.Template{
	"device_picker":   template.Must(template.ParseFS(templatesFS, "templates/partials/device_picker.html")),
	"room_picker":     template.Must(template.ParseFS(templatesFS, "templates/partials/room_picker.html")),
	"device_rename":   template.Must(template.ParseFS(templatesFS, "templates/partials/device_rename.html")),
	"device_info":     template.Must(template.ParseFS(templatesFS, "templates/partials/device_info.html")),
	"policy_status":   template.Must(template.New("").Funcs(policyTemplateFuncs).ParseFS(templatesFS, "templates/partials/policy_status.html")),
	"log_row":         template.Must(template.New("").Funcs(policyTemplateFuncs).ParseFS(templatesFS, "templates/partials/log_row.html")),
	"policy_simulate": template.Must(template.ParseFS(templatesFS, "templates/partials/policy_simulate.html")),
}

// navFuncs are needed by every page, not just the policy ones - layout.html's
// nav (shared by all of them) calls policyAvailable to grey out the
// Policies/Execution Log links when adminui.policy_addr isn't configured.
var navFuncs = template.FuncMap{
	"policyAvailable": func() bool { return policyConfigured.Load() },
}

func mustParsePage(files ...string) *template.Template {
	return mustParsePageFuncs(nil, files...)
}

func mustParsePageFuncs(funcs template.FuncMap, files ...string) *template.Template {
	all := append([]string{"templates/layout.html"}, files...)
	merged := template.FuncMap{}
	for k, v := range navFuncs {
		merged[k] = v
	}
	for k, v := range funcs {
		merged[k] = v
	}
	return template.Must(template.New("layout.html").Funcs(merged).ParseFS(templatesFS, all...))
}
