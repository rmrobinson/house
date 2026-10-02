package main

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/houseview"
	"github.com/rmrobinson/house/service/lib/htmxutil"
)

// policyConfigured reports whether the current Server generation has a
// policy client - read by navFuncs' policyAvailable (templates.go) so every
// page's nav can grey out the Policies/Execution Log links. It is set by
// app.newApp (the first generation) and app.rebuild (settings.go, every
// generation after a Settings save) only once that generation is actually
// the one serving requests - never by newServer itself, which runs before a
// rebuild's persistence step: setting it there would let a rebuild that
// dials successfully but then fails to persist leave this reflecting the
// abandoned generation instead of the old one that's still live.
//
// This is a package-level var rather than a *Server field because
// html/template's FuncMap is fixed when each page's *template.Template is
// parsed at package init (see templates.go's `pages` var), before any
// *Server exists to close over - adminui only ever runs one Server per
// process at a time (rebuild fully swaps generations, never runs two), so a
// singleton is safe. house_addr has no equivalent var: it's mandatory (see
// main.go), so there's no "not configured" state for it to track.
var policyConfigured atomic.Bool

// Server holds the gRPC clients one "generation" of this admin UI depends on
// and implements every route's business logic. Every page render reads
// current state fresh from HouseService/BridgeService on each request,
// matching v1's single-editor scope (see admin-ui-implementation.md) - the
// one exception is hub, which holds the single shared BridgeService.
// StreamUpdates subscription every SSE client is fanned out from (see
// hub.go/sse.go).
//
// A *Server is immutable once built - endpoints, conns, and cancelHubs exist
// only so app.rebuild (settings.go) can tear this generation down after
// building and swapping in a new one, when the Settings page changes
// adminui.house_addr/bridge_facade_addr/policy_addr live. Route registration
// itself lives on *app (app.go), not here: it has to indirect through
// whichever generation is current at request time rather than close over one
// fixed *Server, since a rebuild happens without an app restart.
type Server struct {
	logger    *zap.Logger
	house     api2.HouseServiceClient
	bridge    api2.BridgeServiceClient
	policy    api2.PolicyServiceClient
	hub       *deviceHub
	policyHub *policyHub

	endpoints  endpoints
	conns      []*grpc.ClientConn
	cancelHubs func()
}

// newServer starts the shared device/policy update hubs, which run until
// cancelHubs (set by the caller - see dialServer) is called, and returns the
// Server wrapping them. policySvc may be nil - adminui.policy_addr is
// optional; the /policies and /logs routes still register, but requirePolicy
// renders the policy_unavailable page (with setup instructions) instead of
// calling into a nil client for every one of them.
func newServer(ctx context.Context, logger *zap.Logger, house api2.HouseServiceClient, bridge api2.BridgeServiceClient, policySvc api2.PolicyServiceClient) *Server {
	hub := newDeviceHub(logger, bridge)
	go hub.Run(ctx, "bridge update stream ended, reconnecting")

	var pHub *policyHub
	if policySvc != nil {
		pHub = newPolicyHub(logger, policySvc)
		go pHub.Run(ctx, "policy event stream ended, reconnecting")
	}

	return &Server{logger: logger, house: house, bridge: bridge, policy: policySvc, hub: hub, policyHub: pHub}
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/buildings", http.StatusSeeOther)
}

// renderPage renders page's full document (layout+content) - top-level
// navigation between pages is plain <a href> (not htmx-boosted); only
// in-page actions (create/update/delete/link/unlink) go through htmx and
// use respond/renderFragment instead.
func (s *Server) renderPage(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages[page].ExecuteTemplate(w, "layout", data); err != nil {
		s.logger.Error("template render failed", zap.String("page", page), zap.Error(err))
	}
}

// respond re-renders page's content fragment (for an htmx action response,
// never a full document - actions are only ever triggered by htmx) plus an
// out-of-band swap of the #flash banner, and clears any open picker
// placeholder so a picker closes once its action completes. flash may be
// empty to clear the banner without showing a new message.
func (s *Server) respond(w http.ResponseWriter, page string, data any, flash string, isError bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages[page].ExecuteTemplate(w, "content", data); err != nil {
		s.logger.Error("template render failed", zap.String("page", page), zap.Error(err))
	}
	htmxutil.WriteFlash(w, flash, isError)
	s.clearPicker(w)
}

// renderFragment renders a standalone partial (a picker) with no layout and
// no flash/picker OOB swaps - used for GET responses that populate a picker
// placeholder, as opposed to respond, which is for action responses. name
// must be both fragments' map key and the {{define "name"}} inside its
// template file - a bare .Execute wouldn't run a define block, only a
// template matching the parsed file's own name.
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := fragments[name].ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("template render failed", zap.String("fragment", name), zap.Error(err))
	}
}

// clearPicker closes any open device/room picker after an action completes,
// via an out-of-band swap of the shared #picker placeholder back to empty.
func (s *Server) clearPicker(w http.ResponseWriter) {
	fmt.Fprint(w, `<div id="picker" hx-swap-oob="true"></div>`)
}

// redirectAfterDelete tells htmx to navigate the whole browser to target -
// used after a Building/Floor/Room delete succeeds, since the page the
// request came from no longer has anything to re-render.
func redirectAfterDelete(w http.ResponseWriter, target string) {
	w.Header().Set("HX-Redirect", target)
}

// httpError renders a plain error page/fragment - used only for failures
// unrelated to a specific page's own data (e.g. the initial GetX in a GET
// handler failing), where there's nothing sensible to re-render.
func (s *Server) httpError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("request failed", zap.String("path", r.URL.Path), zap.Error(err))
	http.Error(w, houseview.Message(err), houseview.HTTPStatus(err))
}
