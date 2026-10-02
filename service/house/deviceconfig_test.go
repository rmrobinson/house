package house

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/house/db"
)

// fakeBridgeServer is a minimal api2.BridgeServiceServer stand-in -
// DeviceConfigOverlay only ever calls GetDevice/ListDevices/StreamUpdates on
// inner directly, and forwards ExecuteCommand/ExecuteCommandAsync/GetBridge
// unmodified - so every other method panics if a test reaches it. Every
// Device it hands back is proto.Clone'd, matching the real invariant every
// actual BridgeServiceServer in this repo upholds (facade.Facade.present,
// service/bridge.Service.getDevice/getDevices - see AGENTS.md's "Protobuf
// messages" convention) - DeviceConfigOverlay mutates the Device it's
// handed in place, relying on that invariant to hold.
type fakeBridgeServer struct {
	devices map[string]*apiDevice.Device

	updates []*api2.Update
}

func (f *fakeBridgeServer) GetBridge(ctx context.Context, req *api2.GetBridgeRequest) (*api2.Bridge, error) {
	return &api2.Bridge{Id: "b1"}, nil
}

func (f *fakeBridgeServer) ListDevices(ctx context.Context, req *api2.ListDevicesRequest) (*api2.ListDevicesResponse, error) {
	resp := &api2.ListDevicesResponse{}
	for _, d := range f.devices {
		resp.Devices = append(resp.Devices, proto.Clone(d).(*apiDevice.Device))
	}
	return resp, nil
}

func (f *fakeBridgeServer) GetDevice(ctx context.Context, req *api2.GetDeviceRequest) (*apiDevice.Device, error) {
	d, ok := f.devices[req.GetId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "device id not found")
	}
	return proto.Clone(d).(*apiDevice.Device), nil
}

func (f *fakeBridgeServer) UpdateDeviceConfig(ctx context.Context, req *api2.UpdateDeviceConfigRequest) (*apiDevice.Device, error) {
	panic("not implemented in fake - DeviceConfigOverlay must never forward this upstream")
}

func (f *fakeBridgeServer) ExecuteCommand(ctx context.Context, cmd *command.Command) (*apiDevice.Device, error) {
	return proto.Clone(f.devices[cmd.GetDeviceId()]).(*apiDevice.Device), nil
}

func (f *fakeBridgeServer) ExecuteCommandAsync(ctx context.Context, cmd *command.Command) (*api2.ExecuteCommandAsyncResponse, error) {
	return &api2.ExecuteCommandAsyncResponse{}, nil
}

func (f *fakeBridgeServer) StreamUpdates(req *api2.StreamUpdatesRequest, stream api2.BridgeService_StreamUpdatesServer) error {
	for _, u := range f.updates {
		if err := stream.Send(u); err != nil {
			return err
		}
	}
	return nil
}

// fakeStreamUpdatesServer is a minimal api2.BridgeService_StreamUpdatesServer
// for exercising overlayUpdateStream without a real network connection.
type fakeStreamUpdatesServer struct {
	grpc.ServerStream
	ctx context.Context

	sent []*api2.Update
}

func (s *fakeStreamUpdatesServer) Context() context.Context { return s.ctx }

func (s *fakeStreamUpdatesServer) Send(u *api2.Update) error {
	s.sent = append(s.sent, u)
	return nil
}

// newTestOverlay wires a DeviceConfigOverlay against a fresh in-memory
// database and inner. MaxOpenConns is pinned to 1 - see newTestService's
// doc comment in service_test.go for why.
func newTestOverlay(t *testing.T, inner *fakeBridgeServer) *DeviceConfigOverlay {
	t.Helper()

	sqlDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })

	database, err := db.NewDatabase(zaptest.NewLogger(t), sqlDB)
	require.NoError(t, err)

	return NewDeviceConfigOverlay(zaptest.NewLogger(t), inner, database)
}

func deviceWithName(id, name, version string) *apiDevice.Device {
	return &apiDevice.Device{
		Id:      id,
		Version: version,
		Config:  &apiDevice.Device_Config{Name: name, Description: "stock description"},
	}
}

func TestDeviceConfigOverlay_GetDevice_NoOverride(t *testing.T) {
	inner := &fakeBridgeServer{devices: map[string]*apiDevice.Device{
		"d1": deviceWithName("d1", "Bridge-Reported Name", "v1"),
	}}
	overlay := newTestOverlay(t, inner)

	d, err := overlay.GetDevice(context.Background(), &api2.GetDeviceRequest{Id: "d1"})
	require.NoError(t, err)
	assert.Equal(t, "Bridge-Reported Name", d.GetConfig().GetName())
}

func TestDeviceConfigOverlay_UpdateThenGetDevice_ReturnsOverride(t *testing.T) {
	inner := &fakeBridgeServer{devices: map[string]*apiDevice.Device{
		"d1": deviceWithName("d1", "Bridge-Reported Name", "v1"),
	}}
	overlay := newTestOverlay(t, inner)
	ctx := context.Background()

	updated, err := overlay.UpdateDeviceConfig(ctx, &api2.UpdateDeviceConfigRequest{
		Id:     "d1",
		Config: &apiDevice.Device_Config{Name: "Kitchen Lamp"},
	})
	require.NoError(t, err)
	assert.Equal(t, "Kitchen Lamp", updated.GetConfig().GetName())
	// Description is untouched - only Name is ever overridden.
	assert.Equal(t, "stock description", updated.GetConfig().GetDescription())

	d, err := overlay.GetDevice(ctx, &api2.GetDeviceRequest{Id: "d1"})
	require.NoError(t, err)
	assert.Equal(t, "Kitchen Lamp", d.GetConfig().GetName())

	// The underlying device's own reported config is untouched - the
	// override is layered on read, never written back to the "device".
	assert.Equal(t, "Bridge-Reported Name", inner.devices["d1"].GetConfig().GetName())
}

func TestDeviceConfigOverlay_UpdateDeviceConfig_UnknownDevice(t *testing.T) {
	overlay := newTestOverlay(t, &fakeBridgeServer{devices: map[string]*apiDevice.Device{}})

	_, err := overlay.UpdateDeviceConfig(context.Background(), &api2.UpdateDeviceConfigRequest{
		Id:     "missing",
		Config: &apiDevice.Device_Config{Name: "New Name"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestDeviceConfigOverlay_UpdateDeviceConfig_VersionMismatch(t *testing.T) {
	inner := &fakeBridgeServer{devices: map[string]*apiDevice.Device{
		"d1": deviceWithName("d1", "Bridge-Reported Name", "v1"),
	}}
	overlay := newTestOverlay(t, inner)

	_, err := overlay.UpdateDeviceConfig(context.Background(), &api2.UpdateDeviceConfigRequest{
		Id:      "d1",
		Version: "stale-version",
		Config:  &apiDevice.Device_Config{Name: "Kitchen Lamp"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestDeviceConfigOverlay_ListDevices_AppliesBulkOverride(t *testing.T) {
	inner := &fakeBridgeServer{devices: map[string]*apiDevice.Device{
		"d1": deviceWithName("d1", "Bridge Name 1", "v1"),
		"d2": deviceWithName("d2", "Bridge Name 2", "v1"),
	}}
	overlay := newTestOverlay(t, inner)
	ctx := context.Background()

	_, err := overlay.UpdateDeviceConfig(ctx, &api2.UpdateDeviceConfigRequest{
		Id:     "d1",
		Config: &apiDevice.Device_Config{Name: "Renamed"},
	})
	require.NoError(t, err)

	resp, err := overlay.ListDevices(ctx, &api2.ListDevicesRequest{})
	require.NoError(t, err)

	byID := map[string]string{}
	for _, d := range resp.GetDevices() {
		byID[d.GetId()] = d.GetConfig().GetName()
	}
	assert.Equal(t, "Renamed", byID["d1"])
	assert.Equal(t, "Bridge Name 2", byID["d2"])
}

func TestDeviceConfigOverlay_StreamUpdates_OverlaysDeviceUpdates(t *testing.T) {
	inner := &fakeBridgeServer{
		devices: map[string]*apiDevice.Device{
			"d1": deviceWithName("d1", "Bridge Name", "v1"),
		},
		updates: []*api2.Update{
			{
				Action: api2.Update_INITIAL,
				Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
					DeviceId: "d1",
					Device:   deviceWithName("d1", "Bridge Name", "v1"),
				}},
			},
		},
	}
	overlay := newTestOverlay(t, inner)
	ctx := context.Background()

	_, err := overlay.UpdateDeviceConfig(ctx, &api2.UpdateDeviceConfigRequest{
		Id:     "d1",
		Config: &apiDevice.Device_Config{Name: "Renamed"},
	})
	require.NoError(t, err)

	stream := &fakeStreamUpdatesServer{ctx: ctx}
	require.NoError(t, overlay.StreamUpdates(&api2.StreamUpdatesRequest{}, stream))

	require.Len(t, stream.sent, 1)
	assert.Equal(t, "Renamed", stream.sent[0].GetDeviceUpdate().GetDevice().GetConfig().GetName())
}
