package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	esphome "github.com/richard87/esphome-apiclient"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

const fakeLightKey uint32 = 123456789

// startTestBridge wires up an EsphomeBridge against the given fake node address, using an
// in-process bridge.Service — no gRPC listener is needed since the tests call svc.API() methods
// directly, the same methods bridgecli/service/house would reach over the wire.
func startTestBridge(t *testing.T, addr string) *bridge.Service {
	t.Helper()

	logger := zaptest.NewLogger(t)
	svc := bridge.NewService(logger)

	eb := NewEsphomeBridge(logger, svc, []nodeConfig{
		{
			MAC:     "AA:BB:CC:DD:EE:FF",
			Address: addr,
			Devices: []deviceConfig{
				{
					ID:    "office-lamp",
					Type:  "light",
					Roles: map[string]string{"light": "office_lamp"},
				},
			},
		},
	})
	svc.RegisterHandler(eb, eb.Bridge())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	eb.Start(ctx)

	return svc
}

// waitForDevice polls ListDevices until deviceID appears, or fails the test after timeout.
func waitForDevice(t *testing.T, svc *bridge.Service, deviceID string, timeout time.Duration) *device.Device {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := svc.API().ListDevices(context.Background(), &api2.ListDevicesRequest{})
		if err == nil {
			for _, d := range resp.Devices {
				if d.Id == deviceID {
					return d
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("device %q did not appear within %s", deviceID, timeout)
	return nil
}

// isDeviceReachable reports whether deviceID currently exists and is marked reachable. A missing
// device is treated as unreachable rather than failing the test — callers polling for a
// disconnect/reconnect transition only care about the reachable/unreachable boundary.
func isDeviceReachable(t *testing.T, svc *bridge.Service, deviceID string) bool {
	t.Helper()

	resp, err := svc.API().ListDevices(context.Background(), &api2.ListDevicesRequest{})
	if err != nil {
		return false
	}
	for _, d := range resp.Devices {
		if d.Id == deviceID {
			return d.GetAddress().GetIsReachable()
		}
	}
	return false
}

func TestESPHomeBridge_ConnectDiscoverAndCommand(t *testing.T) {
	server := newFakeESPHomeServer(t, []fakeEntity{
		{objectID: "office_lamp", name: "Office Lamp", key: fakeLightKey},
	})
	svc := startTestBridge(t, server.Addr())

	d := waitForDevice(t, svc, "office-lamp", 5*time.Second)
	require.NotNil(t, d.GetLight(), "device should have been built as a Light")
	assert.True(t, d.GetAddress().GetIsReachable())

	_, err := svc.API().ExecuteCommand(context.Background(), &command.Command{
		DeviceId: "office-lamp",
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	require.NoError(t, err)

	// ExecuteCommand returns as soon as its write syscall completes, not once the fake server has
	// actually read and recorded the frame — so give the server's own goroutine a moment.
	require.Eventually(t, func() bool { return len(server.Commands()) == 1 }, time.Second, 10*time.Millisecond)
	cmds := server.Commands()
	require.Len(t, cmds, 1)
	assert.Equal(t, fakeLightKey, cmds[0].Key)
	assert.True(t, cmds[0].HasState)
	assert.True(t, cmds[0].State)

	d = waitForDevice(t, svc, "office-lamp", time.Second)
	assert.True(t, d.GetLight().GetOnOff().GetState().GetIsOn())

	_, err = svc.API().ExecuteCommand(context.Background(), &command.Command{
		DeviceId: "office-lamp",
		Details:  &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: 42}},
	})
	require.NoError(t, err)

	d = waitForDevice(t, svc, "office-lamp", time.Second)
	assert.EqualValues(t, 42, d.GetLight().GetBrightness().GetState().GetLevel())
}

// TestESPHomeBridge_ReconnectRematchesEntitiesWithoutObjectID is a regression test for the entity
// matching bug found while manually testing this bridge (2026-07-24): on reconnect, the vendored
// esphome-apiclient client library ends up (incorrectly) advertising API 1.14+ support to the
// node, and real ESPHome nodes stop sending object_id at all once a client claims that — the fake
// server here replicates that exact behavior (see fakenode_test.go). Without node.go's
// deriveObjectID fallback, this device would silently fail to re-match after the very first
// reconnect, and every command sent to it afterward would return bridge.ErrDeviceNotFound.
func TestESPHomeBridge_ReconnectRematchesEntitiesWithoutObjectID(t *testing.T) {
	server := newFakeESPHomeServer(t, []fakeEntity{
		{objectID: "office_lamp", name: "Office Lamp", key: fakeLightKey},
	})
	svc := startTestBridge(t, server.Addr())

	waitForDevice(t, svc, "office-lamp", 5*time.Second)

	server.ForceDisconnect()

	// First confirm the disconnect was actually detected before waiting for it to clear. Without
	// this, the very first poll below could run before the client's read loop has even noticed
	// the closed socket, still see the stale pre-disconnect IsReachable=true, and return
	// immediately — without ever exercising a real reconnect.
	require.Eventually(t, func() bool {
		return !isDeviceReachable(t, svc, "office-lamp")
	}, 5*time.Second, 20*time.Millisecond, "device should be marked unreachable once the node disconnects")

	// IsReachable is set back to true only from inside onConnect, and only after that specific
	// reconnect's entity matching succeeded (see node.go: a device whose roles fail to match is
	// left untouched — still marked unreachable from handleDisconnect — rather than republished).
	// That makes it a direct, reliable signal that matching succeeded, unlike ExecuteCommand
	// succeeding on its own: applyCommand applies optimistically and can return a nil error even
	// while writing into a socket whose peer just closed, right around ForceDisconnect.
	//
	// The client's own reconnect backoff (node.go hardcodes 5s) plus handshake/discovery time
	// needs to fit inside this window; 20s leaves comfortable margin over the 5s backoff alone.
	require.Eventually(t, func() bool {
		return isDeviceReachable(t, svc, "office-lamp")
	}, 20*time.Second, 200*time.Millisecond, "device should become reachable again once the node reconnects and re-matches")

	_, err := svc.API().ExecuteCommand(context.Background(), &command.Command{
		DeviceId: "office-lamp",
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	require.NoError(t, err, "device should be controllable immediately after reconnecting")

	require.Eventually(t, func() bool { return len(server.Commands()) > 0 }, time.Second, 10*time.Millisecond,
		"the reconnected node should have received the command")
	cmds := server.Commands()
	assert.Equal(t, fakeLightKey, cmds[len(cmds)-1].Key)
}

// TestNodeConn_ApplyCommand_WrapsRawClientErrorsAsGRPCStatus is a regression test for
// nc.applyCommand wrapping raw esphome-apiclient errors (e.g. a write against a dead connection)
// in a gRPC status error before they reach ExecuteCommand's caller, per this repo's convention
// (see AGENTS.md) that gRPC-facing code never returns bare errors.
//
// This drives nodeConn.applyCommand directly (rather than through the full bridge/fake-server
// stack) against a client that's been explicitly Close()d, which deterministically fails the next
// write — going through a live connection and killing it mid-flight (e.g. via the fake server's
// ForceDisconnect) is inherently racy, since a single write to a socket whose peer just closed can
// still succeed locally before the OS notices.
func TestNodeConn_ApplyCommand_WrapsRawClientErrorsAsGRPCStatus(t *testing.T) {
	server := newFakeESPHomeServer(t, nil)
	client := dialFakeServerClient(t, server)
	require.NoError(t, client.Close())

	d, err := lightBuilder{}.build(deviceConfig{ID: "l1", Type: "light"}, map[string]esphome.Entity{
		"light": &esphome.LightEntity{Key: fakeLightKey},
	})
	require.NoError(t, err)

	nc := newNodeConn(zaptest.NewLogger(t), bridge.NewService(zaptest.NewLogger(t)), &EsphomeBridge{}, nodeConfig{})
	nc.client = client
	nc.devices["l1"] = &builtDevice{device: d, builder: lightBuilder{}, roleKeys: map[string]uint32{"light": fakeLightKey}}

	_, err = nc.applyCommand(&command.Command{
		DeviceId: "l1",
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok, "error returned from applyCommand must be a gRPC status error, got %T: %v", err, err)
	assert.Equal(t, codes.Internal, st.Code())
}
