// Package facade implements the BridgeService API as an aggregator over one or
// more upstream BridgeService connections, so a single client connection (to
// this service) is enough to observe and control every bridge configured
// here. See README.md for the design.
package facade

import (
	"context"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

var (
	// ErrBridgeNotFound is returned when a requested bridge_id isn't in the cache.
	ErrBridgeNotFound = status.Error(codes.NotFound, "bridge id not found")
	// ErrBridgeUnreachable is returned when the owning bridge for a device is
	// currently disconnected, so a write can't be forwarded.
	ErrBridgeUnreachable = status.Error(codes.Unavailable, "owning bridge is not currently connected")
)

// Facade aggregates one or more upstream BridgeService connections into a
// single downstream-facing BridgeService implementation. It holds no
// third-party protocol knowledge itself - every upstream is already speaking
// BridgeService - so its only job is fan-in, caching, and routing.
//
// The facade is itself one more hop in the multi-hop mesh Device.Address
// already models: every Device it hands to a downstream client (via
// GetDevice/ListDevices/StreamUpdates, or as the return value of a forwarded
// write) has Address rewritten to the facade's own selfAddress with
// hop_count incremented by one, so a client only ever needs to know how to
// reach the facade. The cache itself (f.devices) keeps the untouched
// address/hop_count as reported by the owning upstream bridge - see
// present().
//
// Facade implements api2.BridgeServiceServer.
type Facade struct {
	logger *zap.Logger

	selfID      string
	selfAddress string

	updates *bridge.Source

	mu      sync.Mutex
	bridges map[string]*api2.Bridge   // bridge_id -> last-known Bridge (includes the facade's own selfID entry)
	devices map[string]*device.Device // device_id -> last-known Device, address/hop_count as reported upstream
	owner   map[string]string         // device_id -> owning bridge_id
	byID    map[string]*upstreamConn  // bridge_id -> live upstream connection, once known
}

// New creates a Facade with no upstream connections. Call Connect once per
// configured bridge address to start fanning its updates in.
//
// self is the Bridge identity this facade advertises for itself - minted
// and persisted by the caller the same way an individual bridge mints its
// own Bridge.Id (see cmd/bridgefacaded/main.go) - and selfAddress is the
// network address downstream clients use to reach this facade, published as
// every proxied Device's Address.
func New(logger *zap.Logger, self *api2.Bridge, selfAddress string) *Facade {
	f := &Facade{
		logger:      logger,
		selfID:      self.GetId(),
		selfAddress: selfAddress,
		updates:     bridge.NewSource(logger),
		bridges:     make(map[string]*api2.Bridge),
		devices:     make(map[string]*device.Device),
		owner:       make(map[string]string),
		byID:        make(map[string]*upstreamConn),
	}
	f.bridges[f.selfID] = proto.Clone(self).(*api2.Bridge)
	return f
}

// present returns a clone of d as a downstream client should see it: Address
// rewritten to this facade's own selfAddress, with hop_count incremented by
// one to account for the extra hop through the facade. d itself (the cached,
// upstream-reported original) is left untouched.
func (f *Facade) present(d *device.Device) *device.Device {
	out := proto.Clone(d).(*device.Device)
	out.Address = &device.Device_Address{
		Address:     f.selfAddress,
		IsReachable: d.GetAddress().GetIsReachable(),
		HopCount:    d.GetAddress().GetHopCount() + 1,
	}
	return out
}

// Connect starts (and maintains, with reconnect-on-drop) an upstream
// connection to the BridgeService at addr. It returns immediately; the
// connection is established and streamed in a background goroutine that
// runs until ctx is cancelled.
func (f *Facade) Connect(ctx context.Context, addr string) {
	uc := &upstreamConn{
		addr: addr,
		f:    f,
	}
	uc.backoff.Store(int64(minReconnectBackoff))
	go uc.run(ctx)
}

// snapshotOne returns a clone of m[id], or the zero value if absent, safe
// for concurrent use guarded by mu. Shared by every "get one cached proto
// message" accessor below - they differ only in which map they read.
func snapshotOne[V proto.Message](mu *sync.Mutex, m map[string]V, id string) V {
	mu.Lock()
	defer mu.Unlock()

	v, ok := m[id]
	if !ok {
		var zero V
		return zero
	}
	return proto.Clone(v).(V)
}

// snapshotAll returns a clone of every value in m, safe for concurrent use
// guarded by mu.
func snapshotAll[V proto.Message](mu *sync.Mutex, m map[string]V) []V {
	mu.Lock()
	defer mu.Unlock()

	ret := make([]V, 0, len(m))
	for _, v := range m {
		ret = append(ret, proto.Clone(v).(V))
	}
	return ret
}

func (f *Facade) getBridge(id string) *api2.Bridge {
	return snapshotOne(&f.mu, f.bridges, id)
}

func (f *Facade) getBridges() []*api2.Bridge {
	return snapshotAll(&f.mu, f.bridges)
}

func (f *Facade) getDevice(id string) *device.Device {
	return snapshotOne(&f.mu, f.devices, id)
}

func (f *Facade) getDevices() []*device.Device {
	return snapshotAll(&f.mu, f.devices)
}

// upstreamClientFor returns the live client for the bridge owning deviceID,
// along with that bridge's ID, or an error if the device or its owning
// bridge's connection isn't currently known.
func (f *Facade) upstreamClientFor(deviceID string) (api2.BridgeServiceClient, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	bridgeID, ok := f.owner[deviceID]
	if !ok {
		return nil, "", bridge.ErrDeviceNotFound
	}

	uc, ok := f.byID[bridgeID]
	if !ok {
		return nil, bridgeID, ErrBridgeUnreachable
	}
	client, live := uc.snapshot()
	if !live {
		return nil, bridgeID, ErrBridgeUnreachable
	}
	return client, bridgeID, nil
}

func (f *Facade) GetBridge(ctx context.Context, req *api2.GetBridgeRequest) (*api2.Bridge, error) {
	b := f.getBridge(req.GetId())
	if b == nil {
		return nil, ErrBridgeNotFound
	}
	return b, nil
}

func (f *Facade) ListDevices(ctx context.Context, req *api2.ListDevicesRequest) (*api2.ListDevicesResponse, error) {
	raw := f.getDevices()
	out := make([]*device.Device, len(raw))
	for i, d := range raw {
		out[i] = f.present(d)
	}
	return &api2.ListDevicesResponse{Devices: out}, nil
}

func (f *Facade) GetDevice(ctx context.Context, req *api2.GetDeviceRequest) (*device.Device, error) {
	d := f.getDevice(req.GetId())
	if d == nil {
		return nil, bridge.ErrDeviceNotFound
	}
	return f.present(d), nil
}

// UpdateDeviceConfig forwards the request, unchanged, to the bridge that
// owns req.Id. The resulting cache update is observed asynchronously via
// that bridge's own update stream (see upstream.go) rather than applied
// here directly - the cache is built entirely from Update messages, per
// the facade's design. The returned Device is presented like any other, so
// a caller doesn't see a raw upstream Address on a write's response but not
// on a subsequent read.
func (f *Facade) UpdateDeviceConfig(ctx context.Context, req *api2.UpdateDeviceConfigRequest) (*device.Device, error) {
	client, _, err := f.upstreamClientFor(req.GetId())
	if err != nil {
		return nil, err
	}
	d, err := client.UpdateDeviceConfig(ctx, req)
	if err != nil {
		return nil, err
	}
	return f.present(d), nil
}

// ExecuteCommand forwards cmd, unchanged, to the bridge that owns
// cmd.DeviceId.
func (f *Facade) ExecuteCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	client, _, err := f.upstreamClientFor(cmd.GetDeviceId())
	if err != nil {
		return nil, err
	}
	d, err := client.ExecuteCommand(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return f.present(d), nil
}

// ExecuteCommandAsync forwards cmd, unchanged, to the bridge that owns
// cmd.DeviceId. The terminal CommandUpdate it produces arrives over that
// bridge's own update stream and is republished verbatim to this facade's
// subscribers, same as any other update.
func (f *Facade) ExecuteCommandAsync(ctx context.Context, cmd *command.Command) (*api2.ExecuteCommandAsyncResponse, error) {
	client, _, err := f.upstreamClientFor(cmd.GetDeviceId())
	if err != nil {
		return nil, err
	}
	return client.ExecuteCommandAsync(ctx, cmd)
}

// StreamUpdates never forwards a raw upstream InitialUpdate: a downstream
// client connecting to the facade has no visibility into when each upstream
// bridge last reconnected, so replaying one verbatim would be ambiguous. It
// synthesizes its own initial burst from the cache instead - one
// BridgeUpdate (INITIAL) per known bridge, then one DeviceUpdate (INITIAL)
// per known device - which is well-defined regardless of upstream reconnect
// timing.
func (f *Facade) StreamUpdates(req *api2.StreamUpdatesRequest, stream api2.BridgeService_StreamUpdatesServer) error {
	logger := f.logger

	// Subscribe before snapshotting the cache so no update landing between the
	// snapshot and the subscribe is missed; the client may see a harmless
	// duplicate in that window instead (same tradeoff as service/bridge.API).
	sink := f.updates.NewSink()
	defer sink.Close()

	f.mu.Lock()
	bridges := make([]*api2.Bridge, 0, len(f.bridges))
	for _, b := range f.bridges {
		bridges = append(bridges, proto.Clone(b).(*api2.Bridge))
	}
	devices := make([]*deviceWithOwner, 0, len(f.devices))
	for id, d := range f.devices {
		devices = append(devices, &deviceWithOwner{
			device:   f.present(d),
			bridgeID: f.owner[id],
		})
	}
	f.mu.Unlock()

	for _, b := range bridges {
		update := &api2.Update{
			Action: api2.Update_INITIAL,
			Update: &api2.Update_BridgeUpdate{
				BridgeUpdate: &api2.BridgeUpdate{
					BridgeId: b.GetId(),
					Bridge:   b,
				},
			},
		}
		if err := stream.Send(update); err != nil {
			logger.Error("failed to send initial bridge state", zap.Error(err))
			return err
		}
	}
	for _, dw := range devices {
		update := &api2.Update{
			Action: api2.Update_INITIAL,
			Update: &api2.Update_DeviceUpdate{
				DeviceUpdate: &api2.DeviceUpdate{
					BridgeId: dw.bridgeID,
					DeviceId: dw.device.GetId(),
					Device:   dw.device,
				},
			},
		}
		if err := stream.Send(update); err != nil {
			logger.Error("failed to send initial device state", zap.Error(err))
			return err
		}
	}

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case msg, ok := <-sink.Messages():
			if !ok {
				return nil
			}
			update, castOk := msg.(*api2.Update)
			if !castOk {
				panic("must send api2.Update messages to the updates chan")
			}
			if err := stream.Send(update); err != nil {
				logger.Error("unable to send update", zap.Error(err))
				return err
			}
		}
	}
}

type deviceWithOwner struct {
	device   *device.Device
	bridgeID string
}
