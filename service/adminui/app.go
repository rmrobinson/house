package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/lib/webassets"
)

// basePath is every route's prefix (empty by default - adminui at the
// site root) - see adminui.base_path in main.go. It's a package-level var
// (set once in newApp, before the mux ever serves a request) rather than an
// *app field for the same reason policyConfigured (server.go) is: it has to
// be readable from templates.go's FuncMap, which is fixed at package init
// time, before any *app exists. Needed whenever this process runs behind a
// reverse proxy that forwards a path prefix (e.g. Caddy routing
// house-config's combined adminui+viewerui host's "/admin" at this process)
// without stripping it first - every route registered below, every
// Go-side redirect (server.go's handleRoot/redirectAfterDelete,
// handlers_device.go's devicesFilter.URL), and every template-side link/
// htmx/SSE URL (templates.go's url/basePath funcs) has to carry this same
// prefix so a link this process renders actually round-trips back to it
// through the proxy, rather than falling through to whatever else the
// proxy routes its own site root to.
//
// Unlike policyConfigured, this never changes once set - adminui only ever
// runs one base path per process, set once from adminui.base_path before the
// first route is registered. basePathSet guards that: a second newApp call
// in the same process with a different bp (e.g. a future test varying it)
// would otherwise silently overwrite basePath out from under whichever
// *app's requests/templates are already relying on the old value, with
// nothing erroring - this turns that into an immediate panic instead.
var (
	basePath    string
	basePathSet bool
)

// app owns the mux and the current *Server generation - the layer that
// makes the Settings page's live reconnect (settings.go) possible. Every
// route in the table below has to look up whichever *Server is current at
// request time rather than close over one fixed *Server, since a rebuild
// (dialing new house/bridge/policy clients and starting new hubs for
// whatever address the Settings page was just saved with) happens without
// restarting the process or its listener.
type app struct {
	logger *zap.Logger

	// rootCtx is never cancelled itself - every generation's hubs run off
	// their own context.WithCancel(rootCtx) (see dialServer), so cancelling
	// one generation on rebuild can never reach another's.
	rootCtx context.Context

	configPath string
	tlsCfg     *grpcutil.ClientTLSConfig

	// rebuildMu serializes rebuild (settings.go) end to end - validate,
	// dial, persist to configPath, swap current - so two concurrent
	// Settings saves can't interleave their persists/swaps.
	rebuildMu sync.Mutex
	current   atomic.Pointer[Server]

	mux *http.ServeMux
}

// newApp builds the route table and the first Server generation from ep,
// wired for live reconnect. Route handlers that only need one specific
// generation's data (e.g. rendering the current Settings form from its
// endpoints) load a.current themselves; routes registered via a.handle
// (every *Server business-logic method) do the same indirection generically.
//
// bp is this process's base path (see basePath above) - "" to serve at the
// site root exactly as before, or a "/"-prefixed, no-trailing-slash prefix
// (e.g. "/admin") to serve every route under it instead.
func newApp(rootCtx context.Context, logger *zap.Logger, configPath string, tlsCfg *grpcutil.ClientTLSConfig, ep endpoints, bp string) (*app, error) {
	if basePathSet && basePath != bp {
		panic(fmt.Sprintf("adminui: basePath already set to %q, cannot change to %q (newApp called twice with different base paths in one process)", basePath, bp))
	}
	basePath = bp
	basePathSet = true
	a := &app{logger: logger, rootCtx: rootCtx, configPath: configPath, tlsCfg: tlsCfg}

	s, err := dialServer(rootCtx, logger, ep, tlsCfg)
	if err != nil {
		return nil, err
	}
	a.current.Store(s)
	policyConfigured.Store(s.policy != nil)

	mux := http.NewServeMux()
	// http.FileServerFS looks a request up by its raw URL path, and
	// staticFS's embedded tree only knows paths like "static/lua.min.js" -
	// no "admin" anywhere in it - so basePath has to be stripped back off
	// before the lookup, not just matched going in.
	mux.Handle("GET "+basePath+"/static/", http.StripPrefix(basePath, http.FileServerFS(staticFS)))
	webassets.Register(mux, basePath)

	mux.HandleFunc("GET "+basePath+"/{$}", a.handle((*Server).handleRoot))
	if basePath != "" {
		// "/{$}" above only matches the trailing-slash form (basePath+"/");
		// a bare basePath with no trailing slash - the URL a person
		// actually types or bookmarks - needs its own exact-match route.
		mux.HandleFunc("GET "+basePath, a.handle((*Server).handleRoot))
	}

	mux.HandleFunc("GET "+basePath+"/buildings", a.handle((*Server).handleBuildingsList))
	mux.HandleFunc("POST "+basePath+"/buildings", a.handle((*Server).handleBuildingCreate))
	mux.HandleFunc("GET "+basePath+"/buildings/{id}", a.handle((*Server).handleBuildingGet))
	mux.HandleFunc("POST "+basePath+"/buildings/{id}", a.handle((*Server).handleBuildingUpdate))
	mux.HandleFunc("POST "+basePath+"/buildings/{id}/delete", a.handle((*Server).handleBuildingDelete))
	mux.HandleFunc("POST "+basePath+"/buildings/{id}/floors", a.handle((*Server).handleFloorCreate))

	mux.HandleFunc("GET "+basePath+"/floors/{id}", a.handle((*Server).handleFloorGet))
	mux.HandleFunc("POST "+basePath+"/floors/{id}", a.handle((*Server).handleFloorUpdate))
	mux.HandleFunc("POST "+basePath+"/floors/{id}/delete", a.handle((*Server).handleFloorDelete))
	mux.HandleFunc("POST "+basePath+"/floors/{id}/rooms", a.handle((*Server).handleRoomCreate))

	mux.HandleFunc("GET "+basePath+"/rooms/{id}", a.handle((*Server).handleRoomGet))
	mux.HandleFunc("POST "+basePath+"/rooms/{id}", a.handle((*Server).handleRoomUpdate))
	mux.HandleFunc("POST "+basePath+"/rooms/{id}/delete", a.handle((*Server).handleRoomDelete))
	mux.HandleFunc("GET "+basePath+"/rooms/{id}/device-picker", a.handle((*Server).handleRoomDevicePicker))
	mux.HandleFunc("POST "+basePath+"/rooms/{id}/link", a.handle((*Server).handleRoomLinkDevice))
	mux.HandleFunc("POST "+basePath+"/rooms/{id}/unlink", a.handle((*Server).handleRoomUnlinkDevice))
	mux.HandleFunc("GET "+basePath+"/rooms/{id}/devices/{deviceID}/rename", a.handle((*Server).handleRoomDeviceRenamePicker))
	mux.HandleFunc("POST "+basePath+"/rooms/{id}/devices/{deviceID}/rename", a.handle((*Server).handleRoomDeviceRename))

	mux.HandleFunc("GET "+basePath+"/devices", a.handle((*Server).handleDevicesList))
	mux.HandleFunc("GET "+basePath+"/devices/{id}/room-picker", a.handle((*Server).handleDeviceRoomPicker))
	mux.HandleFunc("POST "+basePath+"/devices/{id}/link", a.handle((*Server).handleDeviceLink))
	mux.HandleFunc("GET "+basePath+"/devices/{id}/rename", a.handle((*Server).handleDeviceRenamePicker))
	mux.HandleFunc("POST "+basePath+"/devices/{id}/rename", a.handle((*Server).handleDeviceRename))

	mux.HandleFunc("GET "+basePath+"/policies", a.handle(requirePolicy((*Server).handlePoliciesList)))
	mux.HandleFunc("POST "+basePath+"/policies", a.handle(requirePolicy((*Server).handlePolicySubmit)))
	mux.HandleFunc("GET "+basePath+"/policies/new", a.handle(requirePolicy((*Server).handlePolicyEditorNew)))
	mux.HandleFunc("GET "+basePath+"/policies/{id}", a.handle(requirePolicy((*Server).handlePolicyDetail)))
	mux.HandleFunc("GET "+basePath+"/policies/{id}/edit", a.handle(requirePolicy((*Server).handlePolicyEditorEdit)))
	mux.HandleFunc("POST "+basePath+"/policies/{id}/delete", a.handle(requirePolicy((*Server).handlePolicyDelete)))
	mux.HandleFunc("GET "+basePath+"/policies/{id}/simulate", a.handle(requirePolicy((*Server).handlePolicySimulate)))
	mux.HandleFunc("GET "+basePath+"/logs", a.handle(requirePolicy((*Server).handleLogs)))

	mux.HandleFunc("GET "+basePath+"/events", a.handle((*Server).handleSSE))

	mux.HandleFunc("GET "+basePath+"/settings", a.handleSettingsGet)
	mux.HandleFunc("POST "+basePath+"/settings", a.handleSettingsSave)

	a.mux = mux
	return a, nil
}

// handle adapts a (*Server) method expression - e.g. (*Server).
// handleBuildingsList - into a plain http.HandlerFunc that dispatches
// against whichever Server generation is current when the request arrives,
// rather than the one that was current when the route was registered.
func (a *app) handle(h func(*Server, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(a.current.Load(), w, r)
	}
}

// requirePolicy is requirePolicy from server.go, adapted to a method
// expression so it composes with a.handle the same way every other route
// does - see that function's doc comment for why the nil check exists.
func requirePolicy(h func(*Server, http.ResponseWriter, *http.Request)) func(*Server, http.ResponseWriter, *http.Request) {
	return func(s *Server, w http.ResponseWriter, r *http.Request) {
		if s.policy == nil {
			s.renderPage(w, "policy_unavailable", nil)
			return
		}
		h(s, w, r)
	}
}
