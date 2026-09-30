package main

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/lib/webassets"
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
func newApp(rootCtx context.Context, logger *zap.Logger, configPath string, tlsCfg *grpcutil.ClientTLSConfig, ep endpoints) (*app, error) {
	a := &app{logger: logger, rootCtx: rootCtx, configPath: configPath, tlsCfg: tlsCfg}

	s, err := dialServer(rootCtx, logger, ep, tlsCfg)
	if err != nil {
		return nil, err
	}
	a.current.Store(s)
	policyConfigured.Store(s.policy != nil)

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	webassets.Register(mux)

	mux.HandleFunc("GET /{$}", a.handle((*Server).handleRoot))

	mux.HandleFunc("GET /buildings", a.handle((*Server).handleBuildingsList))
	mux.HandleFunc("POST /buildings", a.handle((*Server).handleBuildingCreate))
	mux.HandleFunc("GET /buildings/{id}", a.handle((*Server).handleBuildingGet))
	mux.HandleFunc("POST /buildings/{id}", a.handle((*Server).handleBuildingUpdate))
	mux.HandleFunc("POST /buildings/{id}/delete", a.handle((*Server).handleBuildingDelete))
	mux.HandleFunc("POST /buildings/{id}/floors", a.handle((*Server).handleFloorCreate))

	mux.HandleFunc("GET /floors/{id}", a.handle((*Server).handleFloorGet))
	mux.HandleFunc("POST /floors/{id}", a.handle((*Server).handleFloorUpdate))
	mux.HandleFunc("POST /floors/{id}/delete", a.handle((*Server).handleFloorDelete))
	mux.HandleFunc("POST /floors/{id}/rooms", a.handle((*Server).handleRoomCreate))

	mux.HandleFunc("GET /rooms/{id}", a.handle((*Server).handleRoomGet))
	mux.HandleFunc("POST /rooms/{id}", a.handle((*Server).handleRoomUpdate))
	mux.HandleFunc("POST /rooms/{id}/delete", a.handle((*Server).handleRoomDelete))
	mux.HandleFunc("GET /rooms/{id}/device-picker", a.handle((*Server).handleRoomDevicePicker))
	mux.HandleFunc("POST /rooms/{id}/link", a.handle((*Server).handleRoomLinkDevice))
	mux.HandleFunc("POST /rooms/{id}/unlink", a.handle((*Server).handleRoomUnlinkDevice))

	mux.HandleFunc("GET /devices", a.handle((*Server).handleDevicesList))
	mux.HandleFunc("GET /devices/{id}/room-picker", a.handle((*Server).handleDeviceRoomPicker))
	mux.HandleFunc("POST /devices/{id}/link", a.handle((*Server).handleDeviceLink))

	mux.HandleFunc("GET /policies", a.handle(requirePolicy((*Server).handlePoliciesList)))
	mux.HandleFunc("POST /policies", a.handle(requirePolicy((*Server).handlePolicySubmit)))
	mux.HandleFunc("GET /policies/new", a.handle(requirePolicy((*Server).handlePolicyEditorNew)))
	mux.HandleFunc("GET /policies/{id}", a.handle(requirePolicy((*Server).handlePolicyDetail)))
	mux.HandleFunc("GET /policies/{id}/edit", a.handle(requirePolicy((*Server).handlePolicyEditorEdit)))
	mux.HandleFunc("POST /policies/{id}/delete", a.handle(requirePolicy((*Server).handlePolicyDelete)))
	mux.HandleFunc("GET /policies/{id}/simulate", a.handle(requirePolicy((*Server).handlePolicySimulate)))
	mux.HandleFunc("GET /logs", a.handle(requirePolicy((*Server).handleLogs)))

	mux.HandleFunc("GET /events", a.handle((*Server).handleSSE))

	mux.HandleFunc("GET /settings", a.handleSettingsGet)
	mux.HandleFunc("POST /settings", a.handleSettingsSave)

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
