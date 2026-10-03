package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/lib/houseview"
	"github.com/rmrobinson/house/service/lib/hub"
	"github.com/rmrobinson/house/service/lib/webassets"
)

// Server holds the gRPC clients and shared update hubs every route uses.
// Unlike adminui there's no live reconnect/Settings generation swap - the
// addresses are fixed at startup.
type Server struct {
	logger *zap.Logger
	house  api2.HouseServiceClient
	bridge api2.BridgeServiceClient

	// iceServersJSON is a JSON array of STUN/TURN urls handed to the camera
	// player's RTCPeerConnection; "[]" (host candidates only) suits a LAN.
	iceServersJSON string

	// ctx is what the hubs run under.
	ctx context.Context

	// devHub fans out BridgeService.StreamUpdates, shared by every tab.
	devHub *hub.Hub[*api2.Update]

	// roomHubs holds one HouseService.StreamHouseUpdates hub per building,
	// started lazily on first SSE subscription (the RPC is per-building).
	roomMu   sync.Mutex
	roomHubs map[string]*hub.Hub[*api2.HouseUpdate]
}

func dialServer(ctx context.Context, logger *zap.Logger, houseAddr, bridgeAddr string, tlsCfg *grpcutil.ClientTLSConfig) (*Server, error) {
	houseConn, err := grpcutil.Dial(houseAddr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("dialing house service at %q: %w", houseAddr, err)
	}
	var bridgeConn *grpc.ClientConn
	if bridgeAddr == houseAddr {
		bridgeConn = houseConn
	} else if bridgeConn, err = grpcutil.Dial(bridgeAddr, tlsCfg); err != nil {
		houseConn.Close()
		return nil, fmt.Errorf("dialing bridge facade at %q: %w", bridgeAddr, err)
	}
	return newServer(ctx, logger, api2.NewHouseServiceClient(houseConn), api2.NewBridgeServiceClient(bridgeConn)), nil
}

func newServer(ctx context.Context, logger *zap.Logger, house api2.HouseServiceClient, bridge api2.BridgeServiceClient) *Server {
	s := &Server{
		logger:         logger,
		house:          house,
		bridge:         bridge,
		ctx:            ctx,
		iceServersJSON: "[]",
		roomHubs:       map[string]*hub.Hub[*api2.HouseUpdate]{},
		devHub: hub.New(logger, func(ctx context.Context) (func() (*api2.Update, error), error) {
			stream, err := bridge.StreamUpdates(ctx, &api2.StreamUpdatesRequest{})
			if err != nil {
				return nil, err
			}
			return stream.Recv, nil
		}),
	}
	go s.devHub.Run(ctx, "bridge update stream ended, reconnecting")
	return s
}

// roomHub returns buildingID's room-update hub, starting it on first use.
func (s *Server) roomHub(buildingID string) *hub.Hub[*api2.HouseUpdate] {
	s.roomMu.Lock()
	defer s.roomMu.Unlock()

	if h, ok := s.roomHubs[buildingID]; ok {
		return h
	}
	h := hub.New(s.logger, func(ctx context.Context) (func() (*api2.HouseUpdate, error), error) {
		stream, err := s.house.StreamHouseUpdates(ctx, &api2.StreamHouseUpdatesRequest{BuildingId: buildingID})
		if err != nil {
			return nil, err
		}
		return stream.Recv, nil
	})
	go h.Run(s.ctx, "house update stream ended, reconnecting")
	s.roomHubs[buildingID] = h
	return h
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	webassets.Register(mux, "")

	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /buildings/{id}", s.handleBuilding)
	mux.HandleFunc("GET /buildings/{id}/floors/{floor_id}", s.handleFloor)
	mux.HandleFunc("GET /buildings/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /rooms/{id}", s.handleRoom)
	mux.HandleFunc("GET /rooms/{id}/camera/{device_id}", s.handleCamera)
	mux.HandleFunc("POST /devices/{id}/commands", s.handleDeviceCommand)
	return protect(mux)
}

// protect adds the headers and request checks every route shares:
//   - clickjacking/sniffing headers, so the toggles can't be framed;
//   - no-store on everything but the vendored static assets, so the back
//     button never shows a stale dashboard;
//   - state-changing requests must carry htmx's HX-Request header. A
//     cross-site <form> can't set a custom header without a CORS preflight,
//     which this server never grants, so this blocks CSRF against the device
//     commands.
func protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("HX-Request") == "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// renderPage renders a full document (layout + content).
func (s *Server) renderPage(w http.ResponseWriter, page string, data any) {
	// Render to a buffer first so a template error is a clean 500 rather
	// than a truncated page behind a 200.
	var buf bytes.Buffer
	if err := pages[page].ExecuteTemplate(&buf, "layout", data); err != nil {
		s.logger.Error("template render failed", zap.String("page", page), zap.Error(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// renderFragment renders one named partial, for an htmx swap.
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := fragments.ExecuteTemplate(&buf, name, data); err != nil {
		s.logger.Error("template render failed", zap.String("fragment", name), zap.Error(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (s *Server) httpError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("request failed", zap.String("path", r.URL.Path), zap.Error(err))
	http.Error(w, houseview.Message(err), houseview.HTTPStatus(err))
}
