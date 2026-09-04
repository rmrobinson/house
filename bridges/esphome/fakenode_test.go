package main

import (
	"net"
	"sync"
	"testing"

	esphome "github.com/richard87/esphome-apiclient"
	"github.com/richard87/esphome-apiclient/pb"
	"google.golang.org/protobuf/proto"
)

// fakeEntity describes a single light entity a fakeESPHomeServer reports via ListEntities.
type fakeEntity struct {
	objectID string
	name     string
	key      uint32
}

// fakeESPHomeServer is a minimal, plaintext-only stand-in for a real ESPHome node's native API
// server. It's built directly on esphome-apiclient's own exported Framer/pb types, so it speaks
// the exact wire format the real client uses without reimplementing any framing logic.
//
// It deliberately replicates one real ESPHome server behavior that a naive fake would miss:
// object_id is only included in ListEntitiesLightResponse for clients that declare API < 1.14
// (see node.go's buildEntityIndex doc comment for the full story). This server always reports
// itself as API 1.14 in HelloResponse — matching a real, current ESPHome node — which is what
// causes the vendored client library to (incorrectly) start advertising 1.14+ support on every
// reconnect after the first. Without that, TestESPHomeBridge_ReconnectRematchesEntitiesWithoutObjectID
// wouldn't actually exercise the regression it's meant to guard against.
type fakeESPHomeServer struct {
	t        *testing.T
	listener net.Listener

	mu         sync.Mutex
	entities   []fakeEntity
	commands   []*pb.LightCommandRequest
	activeConn net.Conn
}

func newFakeESPHomeServer(t *testing.T, entities []fakeEntity) *fakeESPHomeServer {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake esphome server: unable to listen: %v", err)
	}

	s := &fakeESPHomeServer{t: t, listener: lis, entities: entities}
	go s.acceptLoop()
	t.Cleanup(s.Close)

	return s
}

// Addr returns the host:port the fake server is listening on.
func (s *fakeESPHomeServer) Addr() string {
	return s.listener.Addr().String()
}

// Close shuts down the listener and any active connection. Safe to call more than once.
func (s *fakeESPHomeServer) Close() {
	s.listener.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeConn != nil {
		s.activeConn.Close()
		s.activeConn = nil
	}
}

// ForceDisconnect closes the currently active connection, simulating a node reboot or transient
// network drop, so a test can exercise the client's own reconnect logic.
func (s *fakeESPHomeServer) ForceDisconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeConn != nil {
		s.activeConn.Close()
		s.activeConn = nil
	}
}

// Commands returns every LightCommandRequest received so far, across all connections.
func (s *fakeESPHomeServer) Commands() []*pb.LightCommandRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*pb.LightCommandRequest, len(s.commands))
	copy(out, s.commands)
	return out
}

func (s *fakeESPHomeServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}

		s.mu.Lock()
		s.activeConn = conn
		s.mu.Unlock()

		go s.handleConn(conn)
	}
}

func (s *fakeESPHomeServer) handleConn(conn net.Conn) {
	defer conn.Close()
	framer := esphome.NewPlainFramer(conn)

	// The API minor version this specific connection's client declared in its HelloRequest.
	// Real ESPHome tracks this per-connection and uses it to decide whether object_id can be
	// omitted from ListEntities responses; this fake does the same.
	var clientAPIMinor uint32

	for {
		msgType, data, err := framer.ReadFrame()
		if err != nil {
			return
		}

		factory, ok := pb.MessageRegistry[msgType]
		if !ok {
			continue
		}
		msg := factory()
		if err := proto.Unmarshal(data, msg); err != nil {
			return
		}

		switch m := msg.(type) {
		case *pb.HelloRequest:
			clientAPIMinor = m.ApiVersionMinor
			s.send(framer, 2, &pb.HelloResponse{
				ApiVersionMajor: 1,
				ApiVersionMinor: 14,
				ServerInfo:      "fake-esphome-node",
				Name:            "fake-node",
			})

		case *pb.DeviceInfoRequest:
			s.send(framer, 10, &pb.DeviceInfoResponse{
				Name:           "fake-node",
				MacAddress:     "AA:BB:CC:DD:EE:FF",
				EsphomeVersion: "2026.5.2",
			})

		case *pb.ListEntitiesRequest:
			s.mu.Lock()
			entities := append([]fakeEntity(nil), s.entities...)
			s.mu.Unlock()

			for _, e := range entities {
				resp := &pb.ListEntitiesLightResponse{Key: e.key, Name: e.name}
				if clientAPIMinor < 14 {
					resp.ObjectId = e.objectID
				}
				s.send(framer, 15, resp)
			}
			s.send(framer, 19, &pb.ListEntitiesDoneResponse{})

		case *pb.LightCommandRequest:
			s.mu.Lock()
			s.commands = append(s.commands, m)
			s.mu.Unlock()

			resp := &pb.LightStateResponse{Key: m.Key}
			if m.HasState {
				resp.State = m.State
			}
			if m.HasBrightness {
				resp.Brightness = m.Brightness
			}
			s.send(framer, 24, resp)

		case *pb.PingRequest:
			s.send(framer, 8, &pb.PingResponse{})

		case *pb.DisconnectRequest:
			s.send(framer, 6, &pb.DisconnectResponse{})
			return
		}
	}
}

func (s *fakeESPHomeServer) send(framer *esphome.PlainFramer, msgType uint32, msg proto.Message) {
	data, err := proto.Marshal(msg)
	if err != nil {
		s.t.Errorf("fake esphome server: marshal failed: %v", err)
		return
	}
	// A write error here just means the connection was closed (by the test's ForceDisconnect, or
	// by the client) — not a test failure, so it's intentionally not asserted on.
	_ = framer.WriteFrame(msgType, data)
}
