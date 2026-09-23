package facade

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"google.golang.org/protobuf/proto"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

const (
	testSelfBridgeID = "facade-1"
	testSelfAddress  = "facade.local:17020"
)

func newTestFacade(t *testing.T) *Facade {
	return New(zaptest.NewLogger(t), &api2.Bridge{Id: testSelfBridgeID, IsReachable: true}, testSelfAddress)
}

// lightDevice returns a device as an upstream bridge would report it: its
// own address and hop_count, distinct from the facade's, so tests can assert
// present() rewrites them rather than happening to match.
func lightDevice(id string, reachable bool) *device.Device {
	return &device.Device{
		Id: id,
		Address: &device.Device_Address{
			Address:     "10.0.0.9:17010",
			IsReachable: reachable,
			HopCount:    2,
		},
	}
}

func testBridge(id string, reachable bool) *api2.Bridge {
	return &api2.Bridge{
		Id:          id,
		IsReachable: reachable,
	}
}

func recvUpdate(t *testing.T, ch <-chan proto.Message) *api2.Update {
	t.Helper()
	select {
	case msg := <-ch:
		u, ok := msg.(*api2.Update)
		require.True(t, ok)
		return u
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for update")
		return nil
	}
}

func TestIngestInitial_SeedsCacheAndPublishesAdded(t *testing.T) {
	f := newTestFacade(t)
	sink := f.updates.NewSink()
	defer sink.Close()

	uc := &upstreamConn{addr: "bridge-1:1234", f: f}
	f.ingestInitial(uc, &api2.InitialUpdate{
		Bridge:  testBridge("b1", true),
		Devices: []*device.Device{lightDevice("d1", true)},
	})

	assert.True(t, uc.connected())

	b, err := f.GetBridge(context.Background(), &api2.GetBridgeRequest{Id: "b1"})
	require.NoError(t, err)
	assert.Equal(t, "b1", b.GetId())

	d, err := f.GetDevice(context.Background(), &api2.GetDeviceRequest{Id: "d1"})
	require.NoError(t, err)
	assert.Equal(t, "d1", d.GetId())
	// GetDevice presents the facade as the hop, not the upstream bridge.
	assert.Equal(t, testSelfAddress, d.GetAddress().GetAddress())
	assert.Equal(t, int32(3), d.GetAddress().GetHopCount())

	list, err := f.ListDevices(context.Background(), &api2.ListDevicesRequest{})
	require.NoError(t, err)
	assert.Len(t, list.GetDevices(), 1)

	bridgeUpdate := recvUpdate(t, sink.Messages())
	assert.Equal(t, api2.Update_ADDED, bridgeUpdate.Action)
	assert.Equal(t, "b1", bridgeUpdate.GetBridgeUpdate().GetBridgeId())

	deviceUpdate := recvUpdate(t, sink.Messages())
	assert.Equal(t, api2.Update_ADDED, deviceUpdate.Action)
	assert.Equal(t, "d1", deviceUpdate.GetDeviceUpdate().GetDeviceId())
	assert.Equal(t, "b1", deviceUpdate.GetDeviceUpdate().GetBridgeId())
	assert.Equal(t, testSelfAddress, deviceUpdate.GetDeviceUpdate().GetDevice().GetAddress().GetAddress())
	assert.Equal(t, int32(3), deviceUpdate.GetDeviceUpdate().GetDevice().GetAddress().GetHopCount())
}

func TestPresent_RewritesAddressIncrementsHopCountKeepsOriginalCached(t *testing.T) {
	f := newTestFacade(t)

	uc := &upstreamConn{addr: "bridge-1:1234", f: f}
	f.ingestInitial(uc, &api2.InitialUpdate{
		Bridge:  testBridge("b1", true),
		Devices: []*device.Device{lightDevice("d1", true)},
	})

	presented, err := f.GetDevice(context.Background(), &api2.GetDeviceRequest{Id: "d1"})
	require.NoError(t, err)
	assert.Equal(t, testSelfAddress, presented.GetAddress().GetAddress())
	assert.Equal(t, int32(3), presented.GetAddress().GetHopCount())
	assert.True(t, presented.GetAddress().GetIsReachable())

	// The cache keeps the untouched original as reported by the upstream bridge.
	f.mu.Lock()
	raw := f.devices["d1"]
	f.mu.Unlock()
	require.NotNil(t, raw)
	assert.Equal(t, "10.0.0.9:17010", raw.GetAddress().GetAddress())
	assert.Equal(t, int32(2), raw.GetAddress().GetHopCount())
}

func TestGetBridge_ReturnsSelf(t *testing.T) {
	f := newTestFacade(t)

	b, err := f.GetBridge(context.Background(), &api2.GetBridgeRequest{Id: testSelfBridgeID})
	require.NoError(t, err)
	assert.Equal(t, testSelfBridgeID, b.GetId())
	assert.True(t, b.GetIsReachable())
}

func TestIngestInitial_ReconcilesDeviceSetOnReconnect(t *testing.T) {
	f := newTestFacade(t)

	uc := &upstreamConn{addr: "bridge-1:1234", f: f}
	f.ingestInitial(uc, &api2.InitialUpdate{
		Bridge:  testBridge("b1", true),
		Devices: []*device.Device{lightDevice("d1", true), lightDevice("d2", true)},
	})

	sink := f.updates.NewSink()
	defer sink.Close()

	// Reconnect: d1 changes, d2 is gone, d3 is new.
	changedD1 := lightDevice("d1", true)
	changedD1.Config = &device.Device_Config{Name: "kitchen"}
	f.ingestInitial(uc, &api2.InitialUpdate{
		Bridge:  testBridge("b1", true),
		Devices: []*device.Device{changedD1, lightDevice("d3", true)},
	})

	seen := map[string]api2.Update_Action{}
	for i := 0; i < 3; i++ {
		u := recvUpdate(t, sink.Messages())
		du := u.GetDeviceUpdate()
		require.NotNil(t, du)
		seen[du.GetDeviceId()] = u.Action
	}

	assert.Equal(t, api2.Update_CHANGED, seen["d1"])
	assert.Equal(t, api2.Update_REMOVED, seen["d2"])
	assert.Equal(t, api2.Update_ADDED, seen["d3"])

	_, err := f.GetDevice(context.Background(), &api2.GetDeviceRequest{Id: "d2"})
	assert.ErrorIs(t, err, bridge.ErrDeviceNotFound)

	list, err := f.ListDevices(context.Background(), &api2.ListDevicesRequest{})
	require.NoError(t, err)
	assert.Len(t, list.GetDevices(), 2)
}

func TestIngestPassthrough_DeviceUpdate_RewritesAddressButKeepsOwnerAndBridgeID(t *testing.T) {
	f := newTestFacade(t)

	uc := &upstreamConn{addr: "bridge-1:1234", f: f}
	f.ingestInitial(uc, &api2.InitialUpdate{
		Bridge:  testBridge("b1", true),
		Devices: []*device.Device{lightDevice("d1", true)},
	})

	sink := f.updates.NewSink()
	defer sink.Close()

	changed := lightDevice("d1", true)
	changed.Config = &device.Device_Config{Name: "office"}
	f.ingestPassthrough(&api2.Update{
		Action: api2.Update_CHANGED,
		Update: &api2.Update_DeviceUpdate{
			DeviceUpdate: &api2.DeviceUpdate{
				BridgeId: "b1",
				DeviceId: "d1",
				Device:   changed,
			},
		},
	})

	got := recvUpdate(t, sink.Messages())
	assert.Equal(t, api2.Update_CHANGED, got.Action)
	du := got.GetDeviceUpdate()
	require.NotNil(t, du)
	// bridge_id (device ownership/routing) is untouched by the address rewrite.
	assert.Equal(t, "b1", du.GetBridgeId())
	assert.Equal(t, "office", du.GetDevice().GetConfig().GetName())
	assert.Equal(t, testSelfAddress, du.GetDevice().GetAddress().GetAddress())
	assert.Equal(t, int32(3), du.GetDevice().GetAddress().GetHopCount())

	d, err := f.GetDevice(context.Background(), &api2.GetDeviceRequest{Id: "d1"})
	require.NoError(t, err)
	assert.Equal(t, "office", d.GetConfig().GetName())
	assert.Equal(t, testSelfAddress, d.GetAddress().GetAddress())

	// The cache itself keeps the untouched original from upstream.
	f.mu.Lock()
	raw := f.devices["d1"]
	f.mu.Unlock()
	assert.Equal(t, "10.0.0.9:17010", raw.GetAddress().GetAddress())
	assert.Equal(t, int32(2), raw.GetAddress().GetHopCount())
}

func TestMarkUnreachable_FlagsBridgeAndDevicesWithoutDroppingThem(t *testing.T) {
	f := newTestFacade(t)

	uc := &upstreamConn{addr: "bridge-1:1234", f: f}
	f.ingestInitial(uc, &api2.InitialUpdate{
		Bridge:  testBridge("b1", true),
		Devices: []*device.Device{lightDevice("d1", true)},
	})

	sink := f.updates.NewSink()
	defer sink.Close()

	f.markUnreachable("b1")

	bridgeUpdate := recvUpdate(t, sink.Messages())
	assert.Equal(t, api2.Update_CHANGED, bridgeUpdate.Action)
	assert.False(t, bridgeUpdate.GetBridgeUpdate().GetBridge().GetIsReachable())

	deviceUpdate := recvUpdate(t, sink.Messages())
	assert.Equal(t, api2.Update_CHANGED, deviceUpdate.Action)
	assert.False(t, deviceUpdate.GetDeviceUpdate().GetDevice().GetAddress().GetIsReachable())
	assert.Equal(t, testSelfAddress, deviceUpdate.GetDeviceUpdate().GetDevice().GetAddress().GetAddress())

	// Still present in the cache, just unreachable - not dropped.
	b, err := f.GetBridge(context.Background(), &api2.GetBridgeRequest{Id: "b1"})
	require.NoError(t, err)
	assert.False(t, b.GetIsReachable())

	d, err := f.GetDevice(context.Background(), &api2.GetDeviceRequest{Id: "d1"})
	require.NoError(t, err)
	assert.False(t, d.GetAddress().GetIsReachable())
}

func TestExecuteCommand_UnknownDeviceReturnsNotFound(t *testing.T) {
	f := newTestFacade(t)

	_, _, err := f.upstreamClientFor("unknown")
	assert.ErrorIs(t, err, bridge.ErrDeviceNotFound)
}

func TestExecuteCommand_KnownDeviceUnreachableBridge(t *testing.T) {
	f := newTestFacade(t)

	uc := &upstreamConn{addr: "bridge-1:1234", f: f}
	f.ingestInitial(uc, &api2.InitialUpdate{
		Bridge:  testBridge("b1", true),
		Devices: []*device.Device{lightDevice("d1", true)},
	})

	// The upstream connection was never actually dialed in this test, so
	// uc.client is nil / not live from the connection's own perspective even
	// though ingestInitial marked it live=true (as a real Recv loop would
	// have). Simulate the connection dropping.
	f.markUnreachable("b1")
	uc.mu.Lock()
	uc.live = false
	uc.mu.Unlock()

	_, _, err := f.upstreamClientFor("d1")
	assert.ErrorIs(t, err, ErrBridgeUnreachable)
}
