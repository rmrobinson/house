package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/configutil"
	"github.com/rmrobinson/house/service/lib/grpcutil"
)

// endpoints is the subset of adminui's config the Settings page can change
// live: the three gRPC addresses main.go otherwise only reads once at
// startup. TLS (adminui.tls.*) isn't included - it's one cert/key
// identifying this adminui process to every upstream it dials, set once at
// startup and not something a "wrong address" mistake in the field calls
// for changing on the fly the way an endpoint does.
type endpoints struct {
	HouseAddr        string
	BridgeFacadeAddr string
	PolicyAddr       string
}

// dialServer dials ep's addresses and returns a fully-wired *Server for that
// generation - the same dial+newServer sequence main.go's startup path used
// to run inline, now shared with app.rebuild (settings.go) so a Settings
// save produces an identically-built generation instead of a hand-rolled
// second copy of the same logic.
//
// grpc.Dial (via grpcutil.Dial) is lazy - it doesn't verify an address is
// actually reachable before returning, only that it's a well-formed target -
// so this succeeding is not proof the new address works; a bad address just
// surfaces later as an ordinary RPC error on whatever page is opened next,
// same as it always has for house_addr at startup. Settings save reports
// success once dialing and persisting succeed, not once connectivity is
// confirmed.
func dialServer(ctx context.Context, logger *zap.Logger, ep endpoints, tlsCfg *grpcutil.ClientTLSConfig) (*Server, error) {
	if len(ep.HouseAddr) < 1 {
		return nil, errors.New("house address is required")
	}
	bridgeAddr := ep.BridgeFacadeAddr
	if len(bridgeAddr) < 1 {
		bridgeAddr = ep.HouseAddr
	}

	var conns []*grpc.ClientConn
	closeConns := func() {
		for _, c := range conns {
			c.Close()
		}
	}

	houseConn, err := grpcutil.Dial(ep.HouseAddr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("dialing house service at %q: %w", ep.HouseAddr, err)
	}
	conns = append(conns, houseConn)

	bridgeConn, err := grpcutil.Dial(bridgeAddr, tlsCfg)
	if err != nil {
		closeConns()
		return nil, fmt.Errorf("dialing bridge facade at %q: %w", bridgeAddr, err)
	}
	conns = append(conns, bridgeConn)

	var policyClient api2.PolicyServiceClient
	if len(ep.PolicyAddr) > 0 {
		policyConn, err := grpcutil.Dial(ep.PolicyAddr, tlsCfg)
		if err != nil {
			closeConns()
			return nil, fmt.Errorf("dialing policy service at %q: %w", ep.PolicyAddr, err)
		}
		conns = append(conns, policyConn)
		policyClient = api2.NewPolicyServiceClient(policyConn)
	}

	hubCtx, cancel := context.WithCancel(ctx)
	s := newServer(hubCtx, logger, api2.NewHouseServiceClient(houseConn), api2.NewBridgeServiceClient(bridgeConn), policyClient)
	s.endpoints = ep
	s.conns = conns
	s.cancelHubs = cancel
	return s, nil
}

// teardown stops s's hubs and closes its gRPC conns - called on the old
// generation once app.rebuild has swapped a.current to a new one.
// grpc.ClientConn.Close cancels any call still in flight on it immediately,
// so a request that took its own reference to s (via a.current.Load()) just
// before a rebuild swapped it out can see its RPC fail - an accepted
// tradeoff at adminui's admin-tool traffic levels, matching the "single
// editor, no conflict UI" pragmatism elsewhere in this app.
func (s *Server) teardown() {
	if s.cancelHubs != nil {
		s.cancelHubs()
	}
	for _, c := range s.conns {
		c.Close()
	}
}

// rebuild dials ep, and on success persists it to a.configPath and swaps
// a.current to the new generation, tearing the old one down. Serialized by
// rebuildMu so two concurrent Settings saves can't interleave.
func (a *app) rebuild(ep endpoints) error {
	a.rebuildMu.Lock()
	defer a.rebuildMu.Unlock()

	newS, err := dialServer(a.rootCtx, a.logger, ep, a.tlsCfg)
	if err != nil {
		return err
	}

	if err := configutil.PersistValues(a.configPath,
		configutil.KeyValue{KeyPath: "adminui.house_addr", Value: ep.HouseAddr},
		configutil.KeyValue{KeyPath: "adminui.bridge_facade_addr", Value: ep.BridgeFacadeAddr},
		configutil.KeyValue{KeyPath: "adminui.policy_addr", Value: ep.PolicyAddr},
	); err != nil {
		newS.teardown()
		return fmt.Errorf("saving %s: %w", a.configPath, err)
	}

	old := a.current.Swap(newS)
	if old != nil {
		old.teardown()
	}
	return nil
}

type settingsPageData struct {
	endpoints
}

func (a *app) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	s := a.current.Load()
	s.renderPage(w, "settings", settingsPageData{endpoints: s.endpoints})
}

func (a *app) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	s := a.current.Load() // the generation to fall back to rendering with on error

	if err := r.ParseForm(); err != nil {
		s.respond(w, "settings", settingsPageData{endpoints: s.endpoints}, "invalid form: "+err.Error(), true)
		return
	}

	ep := endpoints{
		HouseAddr:        strings.TrimSpace(r.FormValue("house_addr")),
		BridgeFacadeAddr: strings.TrimSpace(r.FormValue("bridge_facade_addr")),
		PolicyAddr:       strings.TrimSpace(r.FormValue("policy_addr")),
	}

	if ep.HouseAddr == "" {
		s.respond(w, "settings", settingsPageData{endpoints: ep}, "house address is required", true)
		return
	}

	if err := a.rebuild(ep); err != nil {
		s.respond(w, "settings", settingsPageData{endpoints: ep}, err.Error(), true)
		return
	}

	// A full browser navigation, not an in-place htmx swap: the open tab's
	// /events connection (templates/layout.html's hx-ext="sse") is still
	// subscribed to the old generation's now-quiescent hub (see
	// Server.teardown), so it has to reconnect - which only happens on a
	// fresh page load, the same way opening a new tab does. Same technique
	// as redirectAfterDelete (server.go) for "nothing sensible to re-render
	// in place", just to this same page instead of elsewhere.
	w.Header().Set("HX-Redirect", "/settings")
}
