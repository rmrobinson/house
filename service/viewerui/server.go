package main

import (
	"context"
	"fmt"
	"net/http"
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

	// ctx is what the hubs run under.
	ctx context.Context

	// devHub fans out BridgeService.StreamUpdates, shared by every tab.
	devHub *hub.Hub[*api2.Update]

	// roomHubs holds one HouseService.StreamHouseUpdates hub per building,
	// started lazily on first SSE subscription (the RPC is per-building).
	roomMu   sync.Mutex
	roomHubs map[string]*hub.Hub[*api2.RoomUpdate]
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
		logger:   logger,
		house:    house,
		bridge:   bridge,
		ctx:      ctx,
		roomHubs: map[string]*hub.Hub[*api2.RoomUpdate]{},
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
func (s *Server) roomHub(buildingID string) *hub.Hub[*api2.RoomUpdate] {
	s.roomMu.Lock()
	defer s.roomMu.Unlock()

	if h, ok := s.roomHubs[buildingID]; ok {
		return h
	}
	h := hub.New(s.logger, func(ctx context.Context) (func() (*api2.RoomUpdate, error), error) {
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

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	webassets.Register(mux)

	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /buildings/{id}", s.handleBuilding)
	mux.HandleFunc("GET /buildings/{id}/floors/{floor_id}", s.handleFloor)
	mux.HandleFunc("GET /buildings/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /rooms/{id}", s.handleRoom)
	mux.HandleFunc("GET /rooms/{id}/camera/{device_id}", s.handleCamera)
	mux.HandleFunc("POST /devices/{id}/commands", s.handleDeviceCommand)
	return mux
}

// renderPage renders a full document (layout + content).
func (s *Server) renderPage(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages[page].ExecuteTemplate(w, "layout", data); err != nil {
		s.logger.Error("template render failed", zap.String("page", page), zap.Error(err))
	}
}

// renderFragment renders one named partial, for an htmx swap.
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := fragments.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("template render failed", zap.String("fragment", name), zap.Error(err))
	}
}

func (s *Server) httpError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("request failed", zap.String("path", r.URL.Path), zap.Error(err))
	http.Error(w, houseview.Message(err), houseview.HTTPStatus(err))
}
