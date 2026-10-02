package house

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
	"github.com/rmrobinson/house/service/house/db"
)

// DeviceConfigOverlay wraps a BridgeService implementation - normally the
// embedded facade.Facade (see cmd/housed/main.go) - so a device's
// Config.Name can be renamed from housed's own admin surface without any
// support from the owning bridge. No bridge in this repo actually
// implements UpdateDeviceConfig (every bridge.Service embeds service/bridge.
// API's stub, which returns Unimplemented - see bridges/cast's deviceConfig.
// Name doc comment), so forwarding the write upstream the way facade.Facade
// forwards every other BridgeService call would just fail everywhere.
// Instead the override is kept in housedb (the device_config table) and
// layered over whatever name the owning bridge reports, on every read -
// surviving a bridge restart/rename and needing no per-protocol support.
//
// DeviceConfigOverlay implements api2.BridgeServiceServer itself, so it can
// be registered on housed's grpc.Server in place of the raw facade - every
// caller (adminui's direct BridgeService dial, and house.Service's own
// loopback bridgeClient) then sees the override transparently.
type DeviceConfigOverlay struct {
	logger *zap.Logger
	inner  api2.BridgeServiceServer
	db     *db.Database
}

// NewDeviceConfigOverlay wraps inner with db-backed device name overrides.
func NewDeviceConfigOverlay(logger *zap.Logger, inner api2.BridgeServiceServer, database *db.Database) *DeviceConfigOverlay {
	return &DeviceConfigOverlay{logger: logger, inner: inner, db: database}
}

// applyOverride replaces d.Config.Name with cfg.Name, if cfg is non-nil.
// d.Config.Description is left untouched either way - only Name is ever
// stored as an override. d is mutated in place: every call site here already
// holds a Device it owns outright (freshly returned from inner, which
// itself only ever hands out proto.Clone'd messages - see facade.Facade's
// doc comment), never a reference shared with some other cache.
func applyOverride(d *device.Device, cfg *db.DeviceConfig) {
	if d == nil || cfg == nil {
		return
	}
	d.Config = &device.Device_Config{
		Name:        cfg.Name,
		Description: d.GetConfig().GetDescription(),
	}
}

// overlayOne fetches deviceID's stored override, if any, and applies it to
// d. A lookup failure is logged and otherwise ignored - d is still returned
// with the bridge-reported name, same tolerant-of-a-missing-override stance
// as a cfg that's simply nil.
func (o *DeviceConfigOverlay) overlayOne(ctx context.Context, d *device.Device) *device.Device {
	if d == nil {
		return d
	}
	cfg, err := o.db.GetDeviceConfig(ctx, d.GetId())
	if err != nil {
		o.logger.Warn("unable to load device name override", zap.String("device_id", d.GetId()), zap.Error(err))
		return d
	}
	applyOverride(d, cfg)
	return d
}

func (o *DeviceConfigOverlay) GetBridge(ctx context.Context, req *api2.GetBridgeRequest) (*api2.Bridge, error) {
	return o.inner.GetBridge(ctx, req)
}

// ListDevices overlays every device in one bulk ListDeviceConfigs query
// rather than one GetDeviceConfig per device - this is the page adminui's
// /devices view hits on every load.
func (o *DeviceConfigOverlay) ListDevices(ctx context.Context, req *api2.ListDevicesRequest) (*api2.ListDevicesResponse, error) {
	resp, err := o.inner.ListDevices(ctx, req)
	if err != nil {
		return nil, err
	}

	overrides, err := o.db.ListDeviceConfigs(ctx)
	if err != nil {
		o.logger.Warn("unable to load device name overrides", zap.Error(err))
		return resp, nil
	}
	for _, d := range resp.GetDevices() {
		if cfg, ok := overrides[d.GetId()]; ok {
			applyOverride(d, &cfg)
		}
	}
	return resp, nil
}

func (o *DeviceConfigOverlay) GetDevice(ctx context.Context, req *api2.GetDeviceRequest) (*device.Device, error) {
	d, err := o.inner.GetDevice(ctx, req)
	if err != nil {
		return nil, err
	}
	return o.overlayOne(ctx, d), nil
}

// UpdateDeviceConfig persists req's name as a housedb override for req.Id,
// rather than forwarding to the owning bridge (see type doc comment). req.
// Id must name a device the inner BridgeService currently knows about -
// otherwise this would let an override accumulate for a device ID that can
// never be seen again. req.Version, if set, is checked against the
// device's current Device.version - the same optimistic-concurrency
// contract documented on UpdateDeviceConfigRequest/Device.version in api/
// device/device.proto - not against anything in housedb, since the
// override itself carries no version of its own (see db.DeviceConfig).
func (o *DeviceConfigOverlay) UpdateDeviceConfig(ctx context.Context, req *api2.UpdateDeviceConfigRequest) (*device.Device, error) {
	d, err := o.inner.GetDevice(ctx, &api2.GetDeviceRequest{Id: req.GetId()})
	if err != nil {
		return nil, err
	}

	if req.GetVersion() != "" && req.GetVersion() != d.GetVersion() {
		return nil, bridge.ErrVersionMismatch
	}

	cfg, err := o.db.SetDeviceConfig(ctx, req.GetId(), req.GetConfig().GetName())
	if err != nil {
		return nil, status.Error(codes.Internal, "unable to save device config")
	}

	applyOverride(d, cfg)
	return d, nil
}

func (o *DeviceConfigOverlay) ExecuteCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	d, err := o.inner.ExecuteCommand(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return o.overlayOne(ctx, d), nil
}

func (o *DeviceConfigOverlay) ExecuteCommandAsync(ctx context.Context, cmd *command.Command) (*api2.ExecuteCommandAsyncResponse, error) {
	return o.inner.ExecuteCommandAsync(ctx, cmd)
}

// StreamUpdates relays inner's stream through overlayUpdateStream, which
// overlays each outgoing Update's device(s) before it reaches the real
// client - a live rename (UpdateDeviceConfig) is then visible on the very
// next Update the client receives for that device, with no separate
// publish step of its own needed.
func (o *DeviceConfigOverlay) StreamUpdates(req *api2.StreamUpdatesRequest, stream api2.BridgeService_StreamUpdatesServer) error {
	return o.inner.StreamUpdates(req, &overlayUpdateStream{BridgeService_StreamUpdatesServer: stream, overlay: o})
}

// overlayUpdateStream wraps a StreamUpdates server-stream so every outgoing
// Update's device(s) are overlaid before Send actually writes them to the
// client - covers both the one-device-per-message shape facade.Facade's
// StreamUpdates sends (BridgeUpdate/DeviceUpdate, one Update per item) and
// the bulk InitialUpdate.Devices shape a raw, non-facade bridge.API.
// StreamUpdates sends, so this works regardless of which one `overlay.inner`
// turns out to be.
type overlayUpdateStream struct {
	api2.BridgeService_StreamUpdatesServer
	overlay *DeviceConfigOverlay
}

func (s *overlayUpdateStream) Send(update *api2.Update) error {
	ctx := s.Context()
	switch u := update.GetUpdate().(type) {
	case *api2.Update_InitialUpdate:
		for _, d := range u.InitialUpdate.GetDevices() {
			s.overlay.overlayOne(ctx, d)
		}
	case *api2.Update_DeviceUpdate:
		s.overlay.overlayOne(ctx, u.DeviceUpdate.GetDevice())
	}
	return s.BridgeService_StreamUpdatesServer.Send(update)
}
