package main

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	esphome "github.com/richard87/esphome-apiclient"
	"github.com/richard87/esphome-apiclient/pb"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

// getTimeRequestID and getTimeResponseID are the ESPHome native API message type IDs for
// GetTimeRequest/GetTimeResponse (see pb.MessageRegistry). The client library doesn't export
// named constants for these the way it does for ListEntitiesDoneResponseID.
const (
	getTimeRequestID  uint32 = 36
	getTimeResponseID uint32 = 37
)

const dialTimeout = 10 * time.Second

// builtDevice tracks everything needed to keep a single house device driven by a node in sync:
// the device itself, the builder that produced it, and the entity key backing each of its roles
// (needed to route outbound commands back to the right ESPHome entity).
type builtDevice struct {
	device   *device.Device
	builder  deviceBuilder
	roleKeys map[string]uint32
}

// nodeConn owns a single ESPHome node connection and every house device built from its entities.
// Connect, reconnect, keepalive and Noise encryption are all handled by the esphome-apiclient
// Client itself; nodeConn's job is entity discovery/matching, state fan-out, and command routing.
type nodeConn struct {
	logger *zap.Logger
	svc    *bridge.Service
	eb     *EsphomeBridge
	cfg    nodeConfig

	client *esphome.Client

	// clientReady is closed once nc.client has been assigned. It exists because the client
	// library invokes WithOnConnect's callback (handleConnect) synchronously from inside
	// DialWithContext for the very first connect, before DialWithContext has returned and
	// nc.client could be assigned — without this gate, that first call would dereference a nil
	// nc.client. handleConnect is therefore always dispatched via a goroutine that waits here
	// first; for every reconnect after the first, nc.client is already set and this is a no-op.
	clientReady chan struct{}

	// subscribed guards against re-registering the state subscription on every reconnect: the
	// client library already re-subscribes automatically using the handler saved from the first
	// call, so calling SubscribeStates again here would stack duplicate, never-cleaned-up
	// handlers on the client's Router (which persists across reconnects) each time the node
	// drops and comes back.
	subscribed bool

	// connectMu serializes onConnect end-to-end. handleConnect dispatches every connect/reconnect
	// callback via its own goroutine with no built-in de-duplication; on a rapidly flapping
	// connection, a slow onConnect (still waiting on ListEntities/DeviceInfo) could otherwise
	// still be running when the next reconnect's onConnect starts, and both would be issuing
	// discovery requests against the one *esphome.Client/Router pair that survives reconnects.
	// Serializing here removes that risk entirely rather than relying on the client library's
	// own request/response teardown timing to keep them from overlapping.
	connectMu sync.Mutex

	// mu guards devices, keyToDeviceID and keyToRole below, plus the *device.Device values they
	// point to. The client library dispatches handleConnect/handleState/handleDisconnect from its
	// own read-loop and reconnect goroutines, while applyCommand runs on the gRPC handler's
	// goroutine — both sides read and mutate the same devices, so this needs real locking rather
	// than the single-event-loop pattern the other bridges in this repo get away with.
	mu sync.Mutex

	// keyToDeviceID and keyToRole let an incoming state message (identified only by entity key)
	// be routed to the house device and role it belongs to.
	keyToDeviceID map[uint32]string
	keyToRole     map[uint32]string
	devices       map[string]*builtDevice
}

func newNodeConn(logger *zap.Logger, svc *bridge.Service, eb *EsphomeBridge, cfg nodeConfig) *nodeConn {
	return &nodeConn{
		logger:        logger.With(zap.String("node_mac", cfg.MAC)),
		svc:           svc,
		eb:            eb,
		cfg:           cfg,
		clientReady:   make(chan struct{}),
		keyToDeviceID: make(map[uint32]string),
		keyToRole:     make(map[uint32]string),
		devices:       make(map[string]*builtDevice),
	}
}

// run dials the node and blocks until ctx is cancelled. The initial dial is retried with a fixed
// backoff since the client's own reconnect logic only takes over after a first successful connect.
// Once connected, reconnects, keepalive and re-subscription are all handled by the Client.
func (nc *nodeConn) run(ctx context.Context) {
	opts := []esphome.Option{
		esphome.WithClientInfo("house-esphome-bridge"),
		esphome.WithReconnect(5 * time.Second),
		esphome.WithOnConnect(nc.handleConnect),
		esphome.WithOnDisconnect(nc.handleDisconnect),
	}
	encrypted := nc.cfg.NoisePSK != ""
	if encrypted {
		opts = append(opts, esphome.WithEncryptionKey(nc.cfg.NoisePSK))
	}
	nc.logger.Info("connecting to node", zap.Bool("noise_encrypted", encrypted))

	for {
		client, err := esphome.DialWithContext(ctx, nc.cfg.Address, dialTimeout, opts...)
		if err != nil {
			nc.logger.Error("unable to connect to node, retrying", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		nc.client = client

		// Registered immediately after nc.client is assigned, well before close(nc.clientReady)
		// and the loop break below, to minimize (the client library's own onConnect callback and
		// read loop are already running by the time DialWithContext returns to us, so this can't
		// be made fully race-free without a library change) the window in which an early
		// GetTimeRequest could arrive with no handler registered yet. Registered once: the
		// client's Router (and anything subscribed on it) survives reconnects, so re-registering
		// this in handleConnect on every reconnect would stack duplicate handlers and send
		// duplicate GetTimeResponses.
		nc.client.On(getTimeRequestID, nc.handleGetTimeRequest)

		close(nc.clientReady)
		break
	}

	<-ctx.Done()
	nc.client.Close()
}

// handleConnect is the WithOnConnect callback: it fires on both the initial connect and every
// subsequent reconnect. The real work happens in onConnect, dispatched via a goroutine gated on
// clientReady — see the clientReady field comment for why this indirection is needed.
func (nc *nodeConn) handleConnect() {
	go nc.onConnect()
}

// onConnect re-discovers entities, re-matches them against the configured devices, and
// (re-)subscribes to state updates. Serialized by connectMu — see its field comment.
func (nc *nodeConn) onConnect() {
	nc.connectMu.Lock()
	defer nc.connectMu.Unlock()

	<-nc.clientReady

	logger := nc.logger

	info, err := nc.client.DeviceInfo()
	if err != nil {
		logger.Error("unable to fetch device info", zap.Error(err))
	} else {
		logger.Info("connected to node",
			zap.String("name", info.Name),
			zap.String("esphome_version", info.EsphomeVersion))
	}

	listed, err := nc.client.ListEntities()
	if err != nil {
		logger.Error("unable to list entities", zap.Error(err))
		return
	}
	entitiesByObjectID := buildEntityIndex(listed)

	nc.mu.Lock()
	for _, dc := range nc.cfg.Devices {
		builder, ok := deviceBuilders[dc.Type]
		if !ok {
			logger.Error("no builder registered for device type", zap.String("device_id", dc.ID), zap.String("type", dc.Type))
			continue
		}

		roleEntities := make(map[string]esphome.Entity, len(dc.Roles))
		roleKeys := make(map[string]uint32, len(dc.Roles))
		missing := false
		for role, objectID := range dc.Roles {
			ent, ok := entitiesByObjectID[objectID]
			if !ok {
				logger.Error("configured entity not found on node",
					zap.String("device_id", dc.ID), zap.String("role", role), zap.String("object_id", objectID))
				missing = true
				continue
			}
			roleEntities[role] = ent
			roleKeys[role] = ent.GetKey()
		}
		if missing {
			continue
		}

		d, err := builder.build(dc, roleEntities)
		if err != nil {
			logger.Error("unable to build device", zap.String("device_id", dc.ID), zap.Error(err))
			continue
		}
		d.Id = dc.ID
		if d.Address == nil {
			d.Address = &device.Device_Address{}
		}
		d.Address.IsReachable = true

		nc.devices[dc.ID] = &builtDevice{device: d, builder: builder, roleKeys: roleKeys}
		for role, ent := range roleEntities {
			key := ent.GetKey()
			if ownerID, claimed := nc.keyToDeviceID[key]; claimed && ownerID != dc.ID {
				logger.Warn("entity claimed by more than one device; state updates will only route to the most recently matched device",
					zap.String("object_id", ent.GetObjectID()), zap.String("previous_device_id", ownerID), zap.String("device_id", dc.ID))
			}
			nc.keyToDeviceID[key] = dc.ID
			nc.keyToRole[key] = role
		}

		nc.eb.registerDevice(dc.ID, nc)
		nc.svc.UpdateDevice(d)
	}
	nc.mu.Unlock()

	if !nc.subscribed {
		if _, err := nc.client.SubscribeStates(nc.handleState); err != nil {
			logger.Error("unable to subscribe to state updates", zap.Error(err))
		} else {
			nc.subscribed = true
		}
	}
}

// handleDisconnect marks every device owned by this node unreachable. Devices remain in the
// store; the client library itself takes care of reconnecting in the background.
func (nc *nodeConn) handleDisconnect() {
	nc.logger.Warn("node disconnected")

	nc.mu.Lock()
	defer nc.mu.Unlock()

	for _, bd := range nc.devices {
		if bd.device.Address == nil {
			bd.device.Address = &device.Device_Address{}
		}
		bd.device.Address.IsReachable = false
		nc.svc.UpdateDevice(bd.device)
	}
}

// handleState dispatches an incoming *StateResponse message to the device/role it belongs to.
func (nc *nodeConn) handleState(msg proto.Message) {
	key, ok := stateMessageKey(msg)
	if !ok {
		return
	}

	nc.mu.Lock()
	defer nc.mu.Unlock()

	deviceID, ok := nc.keyToDeviceID[key]
	if !ok {
		return
	}
	role := nc.keyToRole[key]

	bd := nc.devices[deviceID]
	bd.builder.applyState(bd.device, role, msg)
	nc.svc.UpdateDevice(bd.device)
}

// handleGetTimeRequest responds to a node's standing request for the current time, independent
// of any command having been issued. The timezone comes from the first clock device configured
// on this node, if any; otherwise the bridge process's local timezone is used.
func (nc *nodeConn) handleGetTimeRequest(msg proto.Message) {
	tz := time.Local.String()
	for _, dc := range nc.cfg.Devices {
		if dc.Type == "clock" && dc.Timezone != "" {
			tz = dc.Timezone
			break
		}
	}

	resp := &pb.GetTimeResponse{
		EpochSeconds: uint32(time.Now().UTC().Unix()),
		Timezone:     tz,
	}
	if err := nc.client.SendMessage(resp, getTimeResponseID); err != nil {
		nc.logger.Error("unable to send GetTimeResponse", zap.Error(err))
	}
}

// applyCommand routes a command to the device it targets and, on success, returns the
// optimistically-updated device. The normal state stream corrects this if the node disagrees.
// The returned device is a clone, taken while still holding the lock, so the caller can't race
// with a concurrent state update mutating the same *device.Device.
func (nc *nodeConn) applyCommand(cmd *command.Command) (*device.Device, error) {
	nc.mu.Lock()
	defer nc.mu.Unlock()

	bd, ok := nc.devices[cmd.DeviceId]
	if !ok {
		return nil, bridge.ErrDeviceNotFound
	}

	if err := bd.builder.applyCommand(nc.client, bd.roleKeys, bd.device, cmd); err != nil {
		// Sentinel errors from service/bridge/error.go (e.g. ErrUnsupportedCommand,
		// ErrInvalidTimezone) are already gRPC status errors and pass straight through. Anything
		// else is a raw error from the esphome-apiclient client (e.g. a write failure), which per
		// this repo's convention (see AGENTS.md) shouldn't reach the gRPC layer bare.
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		nc.logger.Error("unable to send command to node", zap.String("device_id", cmd.DeviceId), zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to send command to node")
	}
	return proto.Clone(bd.device).(*device.Device), nil
}

// buildEntityIndex builds a single object_id -> Entity index directly from one ListEntities
// call's response messages. Only entity domains a registered deviceBuilder actually matches
// roles against are indexed here (Light, Switch, Select, Sensor, Fan, Button — see
// deviceBuilders); add a case as new builders start consuming other domains rather than
// pre-populating ones nothing reads yet.
//
// object_id is derived from the entity name via deriveObjectID rather than trusting the wire
// value: ESPHome only sends object_id at all for API-1.10-and-earlier clients (see
// api_connection.cpp's fill_and_encode_entity_info — "API 1.14+ clients compute object_id
// client-side from the entity name"). The esphome-apiclient v1.1.0 client library advertises 1.10
// on its very first HelloRequest, but its handshake() then overwrites its own advertised version
// with whatever the server reports back, so every reconnect after the first ends up claiming
// 1.14+ support — at which point the server stops sending object_id, and this library never
// implements the client-side derivation it just promised. Deriving it ourselves, unconditionally,
// keeps entity matching correct regardless of which of those two paths this particular reconnect
// happened to take.
func buildEntityIndex(listed []proto.Message) map[string]esphome.Entity {
	index := make(map[string]esphome.Entity, len(listed))

	objectID := func(wireObjectID, name string) string {
		if wireObjectID != "" {
			return wireObjectID
		}
		return deriveObjectID(name)
	}

	for _, msg := range listed {
		switch m := msg.(type) {
		case *pb.ListEntitiesLightResponse:
			id := objectID(m.ObjectId, m.Name)
			index[id] = &esphome.LightEntity{Key: m.Key, ObjectID: id, Name: m.Name}
		case *pb.ListEntitiesSwitchResponse:
			id := objectID(m.ObjectId, m.Name)
			index[id] = &esphome.SwitchEntity{Key: m.Key, ObjectID: id, Name: m.Name}
		case *pb.ListEntitiesSelectResponse:
			id := objectID(m.ObjectId, m.Name)
			index[id] = &esphome.SelectEntity{Key: m.Key, ObjectID: id, Name: m.Name, Options: m.Options}
		case *pb.ListEntitiesSensorResponse:
			id := objectID(m.ObjectId, m.Name)
			index[id] = &esphome.SensorEntity{Key: m.Key, ObjectID: id, Name: m.Name, UnitOfMeasurement: m.UnitOfMeasurement}
		case *pb.ListEntitiesFanResponse:
			id := objectID(m.ObjectId, m.Name)
			index[id] = &esphome.FanEntity{
				Key: m.Key, ObjectID: id, Name: m.Name,
				SupportsOscillation: m.SupportsOscillation, SupportsSpeed: m.SupportsSpeed,
				SupportsDirection: m.SupportsDirection, SupportedSpeedCount: m.SupportedSpeedCount,
			}
		case *pb.ListEntitiesButtonResponse:
			id := objectID(m.ObjectId, m.Name)
			index[id] = &esphome.ButtonEntity{Key: m.Key, ObjectID: id, Name: m.Name}
		}
	}

	return index
}

// deriveObjectID replicates ESPHome's own EntityBase::get_object_id_to (core/entity_base.cpp):
// each character is first snake-cased (uppercase -> lowercase, space -> underscore), then
// sanitized (anything other than [A-Za-z0-9_-] becomes an underscore).
func deriveObjectID(name string) string {
	buf := make([]byte, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == ' ' {
			c = '_'
		} else if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			c = '_'
		}
		buf[i] = c
	}
	return string(buf)
}

// stateMessageKey extracts the entity key from any *StateResponse message this bridge handles.
// Only domains buildEntityIndex matches roles against are handled here — see its doc comment.
func stateMessageKey(msg proto.Message) (uint32, bool) {
	switch m := msg.(type) {
	case *pb.LightStateResponse:
		return m.Key, true
	case *pb.SwitchStateResponse:
		return m.Key, true
	case *pb.SelectStateResponse:
		return m.Key, true
	case *pb.SensorStateResponse:
		return m.Key, true
	case *pb.FanStateResponse:
		return m.Key, true
	default:
		return 0, false
	}
}
