package main

import (
	"fmt"
	"html/template"
	"net/http"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
)

// Server holds the two gRPC clients this admin UI depends on and serves
// every route. It holds no other state - every page render reads current
// state fresh from HouseService/BridgeService on each request, matching v1's
// single-editor scope (see admin-ui-implementation.md).
type Server struct {
	logger *zap.Logger
	house  api2.HouseServiceClient
	bridge api2.BridgeServiceClient
	mux    *http.ServeMux
}

func newServer(logger *zap.Logger, house api2.HouseServiceClient, bridge api2.BridgeServiceClient) *Server {
	s := &Server{logger: logger, house: house, bridge: bridge}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	mux.HandleFunc("GET /{$}", s.handleRoot)

	mux.HandleFunc("GET /buildings", s.handleBuildingsList)
	mux.HandleFunc("POST /buildings", s.handleBuildingCreate)
	mux.HandleFunc("GET /buildings/{id}", s.handleBuildingGet)
	mux.HandleFunc("POST /buildings/{id}/delete", s.handleBuildingDelete)
	mux.HandleFunc("POST /buildings/{id}/floors", s.handleFloorCreate)

	mux.HandleFunc("GET /floors/{id}", s.handleFloorGet)
	mux.HandleFunc("POST /floors/{id}", s.handleFloorUpdate)
	mux.HandleFunc("POST /floors/{id}/delete", s.handleFloorDelete)
	mux.HandleFunc("POST /floors/{id}/rooms", s.handleRoomCreate)

	mux.HandleFunc("GET /rooms/{id}", s.handleRoomGet)
	mux.HandleFunc("POST /rooms/{id}/delete", s.handleRoomDelete)
	mux.HandleFunc("GET /rooms/{id}/device-picker", s.handleRoomDevicePicker)
	mux.HandleFunc("POST /rooms/{id}/link", s.handleRoomLinkDevice)
	mux.HandleFunc("POST /rooms/{id}/unlink", s.handleRoomUnlinkDevice)

	mux.HandleFunc("GET /devices", s.handleDevicesList)
	mux.HandleFunc("GET /devices/{id}/room-picker", s.handleDeviceRoomPicker)
	mux.HandleFunc("POST /devices/{id}/link", s.handleDeviceLink)

	mux.HandleFunc("GET /events", s.handleSSE)

	s.mux = mux
	return s
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
	s.writeFlash(w, flash, isError)
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

func (s *Server) writeFlash(w http.ResponseWriter, msg string, isError bool) {
	class := "flash"
	if isError {
		class = "flash flash-error"
	}
	fmt.Fprintf(w, `<div id="flash" hx-swap-oob="true" class="%s">%s</div>`, class, template.HTMLEscapeString(msg))
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
	http.Error(w, grpcMessage(err), grpcHTTPStatus(err))
}

// grpcHTTPStatus maps a gRPC status code (as returned by HouseService/
// BridgeService) to the closest HTTP status for display.
func grpcHTTPStatus(err error) int {
	switch status.Code(err) {
	case codes.NotFound:
		return http.StatusNotFound
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.FailedPrecondition:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// grpcMessage extracts the human-readable message from a gRPC status error,
// falling back to err.Error() for anything else.
func grpcMessage(err error) string {
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}
