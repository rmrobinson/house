package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/service/bridge"
)

func newTestNetworkConn(t *testing.T, cfg zigbeeConfig) (*networkConn, *fakeMQTTClient) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	fc := newFakeMQTTClient()
	if cfg.MQTT.BaseTopic == "" {
		cfg.MQTT.BaseTopic = "zigbee2mqtt"
	}
	mc := &mqttConn{logger: logger, cfg: cfg.MQTT, client: fc, waiters: make(map[string][]chan []byte)}

	svc := bridge.NewService(logger)
	zb := &ZigbeeBridge{
		logger:      logger,
		svc:         svc,
		b:           &api2.Bridge{Id: "test-bridge"},
		deviceOwner: make(map[string]*networkConn),
	}
	nc := newNetworkConn(logger, svc, zb, mc, cfg)
	zb.mqtt = mc
	zb.net = nc
	svc.RegisterHandler(zb, zb.b)

	mc.handleConnect(fc)
	return nc, fc
}

func TestClassify(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zigbeeConfig{})

	b, ok := nc.classify(switchDevice(false))
	require.True(t, ok)
	_, isOnOff := b.(onOffBuilder)
	assert.True(t, isOnOff)

	b, ok = nc.classify(lightDevice(true, true, true))
	require.True(t, ok)
	_, isLight := b.(lightBuilder)
	assert.True(t, isLight)

	b, ok = nc.classify(sensorDevice("temperature"))
	require.True(t, ok)
	_, isSensor := b.(sensorBuilder)
	assert.True(t, isSensor)

	b, ok = nc.classify(starkvindDevice())
	require.True(t, ok)
	_, isFan := b.(fanBuilder)
	assert.True(t, isFan)

	coordinator := bridgeDevice{Type: "Coordinator", Definition: &deviceDefiniton{}}
	_, ok = nc.classify(coordinator)
	assert.False(t, ok, "the coordinator entry itself must never classify as a device")

	disabled := switchDevice(false)
	disabled.Disabled = true
	_, ok = nc.classify(disabled)
	assert.False(t, ok)

	unsupported := bridgeDevice{Type: "EndDevice", Definition: &deviceDefiniton{Exposes: nil}}
	_, ok = nc.classify(unsupported)
	assert.False(t, ok, "a device with no exposes at all must not classify as anything")
}

func TestBuildDevice_RegistersAndRoutesState(t *testing.T) {
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})

	nc.buildDevice(switchDevice(false))

	nc.mu.Lock()
	id, ok := nc.ieeeToDeviceID["0xabc"]
	nc.mu.Unlock()
	require.True(t, ok)
	assert.Equal(t, "zigbee-abc", id)

	// A subsequent state message on the device's own topic must route to it.
	fc.deliver("zigbee2mqtt/plug1", []byte(`{"state":"ON"}`))

	nc.mu.Lock()
	bd := nc.devices[id]
	nc.mu.Unlock()
	bd.mu.Lock()
	isOn := bd.device.GetGeneric().OnOff.State.IsOn
	bd.mu.Unlock()
	assert.True(t, isOn)
}

func TestBuildDevice_IDOverride(t *testing.T) {
	cfg := zigbeeConfig{Devices: []deviceOverride{{IEEEAddress: "0xabc", ID: "kitchen-plug"}}}
	nc, _ := newTestNetworkConn(t, cfg)

	nc.buildDevice(switchDevice(false))

	nc.mu.Lock()
	defer nc.mu.Unlock()
	_, ok := nc.devices["kitchen-plug"]
	assert.True(t, ok)
}

func TestHandleBridgeDevices_DiscoversAndPrunes(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zigbeeConfig{})

	payload, err := json.Marshal([]bridgeDevice{switchDevice(false), lightDevice(true, true, true)})
	require.NoError(t, err)
	nc.handleBridgeDevices(payload)

	nc.mu.Lock()
	assert.Len(t, nc.devices, 2)
	nc.mu.Unlock()

	// A second pass that omits the light must prune it, but keep the switch.
	payload2, err := json.Marshal([]bridgeDevice{switchDevice(false)})
	require.NoError(t, err)
	nc.handleBridgeDevices(payload2)

	nc.mu.Lock()
	defer nc.mu.Unlock()
	assert.Len(t, nc.devices, 1)
	_, stillThere := nc.devices["zigbee-abc"]
	assert.True(t, stillThere)
}

// TestHandleBridgeDevices_PreservesStateAcrossRediscovery is a regression test: bridge/devices
// carries a device's capabilities only, never its current property values, so an earlier version
// of this bridge unconditionally rebuilding every device on every bridge/devices pass reset every
// already-known device's live state back to its zero value any time *any* device on the network
// paired, was removed, or was renamed - not just the device that actually changed. See
// buildDevice's doc comment.
func TestHandleBridgeDevices_PreservesStateAcrossRediscovery(t *testing.T) {
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})

	payload, err := json.Marshal([]bridgeDevice{switchDevice(false)})
	require.NoError(t, err)
	nc.handleBridgeDevices(payload)

	// Apply live state, as if the device's own state update arrived after discovery.
	fc.deliver("zigbee2mqtt/plug1", []byte(`{"state":"ON"}`))

	nc.mu.Lock()
	id := nc.ieeeToDeviceID["0xabc"]
	nc.mu.Unlock()
	nc.mu.Lock()
	bd := nc.devices[id]
	nc.mu.Unlock()
	bd.mu.Lock()
	require.True(t, bd.device.GetGeneric().OnOff.State.IsOn)
	bd.mu.Unlock()

	// A second bridge/devices pass - as would arrive if some *other* device paired, was renamed,
	// or was removed - must not reset this device's already-learned state.
	payload2, err := json.Marshal([]bridgeDevice{switchDevice(false), lightDevice(true, true, true)})
	require.NoError(t, err)
	nc.handleBridgeDevices(payload2)

	nc.mu.Lock()
	bd2 := nc.devices[id]
	nc.mu.Unlock()
	bd2.mu.Lock()
	defer bd2.mu.Unlock()
	assert.True(t, bd2.device.GetGeneric().OnOff.State.IsOn, "rediscovery must not reset an already-known device's live state")
	assert.Same(t, bd, bd2, "an already-known device's *builtDevice must not be replaced by rediscovery")
}

// TestHandleBridgeDevices_RenameUpdatesRouting confirms a friendly_name change - the one thing
// that can legitimately differ for an already-known physical device across rediscovery passes -
// is still picked up, by re-pointing the state-topic routing table at the new topic.
func TestHandleBridgeDevices_RenameUpdatesRouting(t *testing.T) {
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})

	payload, err := json.Marshal([]bridgeDevice{switchDevice(false)})
	require.NoError(t, err)
	nc.handleBridgeDevices(payload)

	renamed := switchDevice(false)
	renamed.FriendlyName = "renamed_plug"
	payload2, err := json.Marshal([]bridgeDevice{renamed})
	require.NoError(t, err)
	nc.handleBridgeDevices(payload2)

	fc.deliver("zigbee2mqtt/renamed_plug", []byte(`{"state":"ON"}`))

	nc.mu.Lock()
	id := nc.ieeeToDeviceID["0xabc"]
	bd := nc.devices[id]
	nc.mu.Unlock()
	bd.mu.Lock()
	defer bd.mu.Unlock()
	assert.True(t, bd.device.GetGeneric().OnOff.State.IsOn, "a state update on the new friendly_name's topic must still route correctly after a rename")
}

// TestBuildDevice_RenameRaceWithApplyCommand is a regression test for a data race an earlier
// version of buildDevice's rename path had: it mutated existingBD.friendlyName directly under
// nc.mu, while applyCommand reads bd.friendlyName under bd.mu - two different locks guarding the
// same field is a race regardless of which side actually loses it, so this only fails reliably
// under `go test -race` (see BUILD.bazel's --@rules_go//go/config:race), not by observing a wrong
// value. Run with `bazel test //bridges/zigbee:zigbee_test --@rules_go//go/config:race`.
func TestBuildDevice_RenameRaceWithApplyCommand(t *testing.T) {
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(switchDevice(false))
	fc.respond("zigbee2mqtt/plug1/set", "zigbee2mqtt/plug1", []byte(`{"state":"ON"}`))
	fc.respond("zigbee2mqtt/plug1_renamed/set", "zigbee2mqtt/plug1_renamed", []byte(`{"state":"ON"}`))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		renamed := switchDevice(false)
		renamed.FriendlyName = "plug1_renamed"
		nc.buildDevice(renamed)
	}()
	go func() {
		defer wg.Done()
		_, _ = nc.applyCommand(context.Background(), &command.Command{
			DeviceId: "zigbee-abc",
			Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
		})
	}()
	wg.Wait()
}

func TestHandleAvailability(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(switchDevice(false))

	nc.handleAvailability("plug1", []byte(`{"state":"offline"}`))

	nc.mu.Lock()
	id := nc.ieeeToDeviceID["0xabc"]
	bd := nc.devices[id]
	nc.mu.Unlock()

	bd.mu.Lock()
	defer bd.mu.Unlock()
	assert.False(t, bd.device.Address.IsReachable)
}

func TestApplyCommand_RoutesToDevice(t *testing.T) {
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(switchDevice(false))
	fc.respond("zigbee2mqtt/plug1/set", "zigbee2mqtt/plug1", []byte(`{"state":"ON"}`))

	nc.mu.Lock()
	id := nc.ieeeToDeviceID["0xabc"]
	nc.mu.Unlock()

	d, err := nc.applyCommand(context.Background(), &command.Command{
		DeviceId: id,
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	require.NoError(t, err)
	assert.True(t, d.GetGeneric().OnOff.State.IsOn)
}

func TestApplyCommand_UnknownDevice(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zigbeeConfig{})
	_, err := nc.applyCommand(context.Background(), &command.Command{DeviceId: "does-not-exist"})
	assert.ErrorIs(t, err, bridge.ErrDeviceNotFound)
}
