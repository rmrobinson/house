package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/service/bridge"
)

func newTestNetworkConn(t *testing.T, cfg zwaveConfig) (*networkConn, *fakeMQTTClient) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	fc := newFakeMQTTClient()
	mc := &mqttConn{logger: logger, cfg: cfg.MQTT, client: fc, waiters: make(map[string][]chan []byte)}

	svc := bridge.NewService(logger)
	zb := &ZwaveBridge{
		logger:      logger,
		svc:         svc,
		b:           &api2.Bridge{Id: "test-bridge"},
		deviceOwner: make(map[string]*networkConn),
	}
	nc := newNetworkConn(logger, svc, zb, mc, cfg)
	zb.mqtt = mc
	zb.net = nc
	svc.RegisterHandler(zb, zb.b)

	// handleConnect fires networkConn.onConnect (wired above via newNetworkConn), which calls
	// getNodes - arm an empty response so that resolves immediately instead of blocking for the
	// full getNodesTimeout. Individual tests call buildNode directly for the nodes they care
	// about, so an empty discovery pass here is harmless.
	apiTopic := mc.apiResponseTopic("getNodes")
	fc.respond(apiTopic+"/set", apiTopic, []byte(`{"success":true,"result":[]}`))

	mc.handleConnect(fc)
	return nc, fc
}

func TestClassify(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zwaveConfig{})

	binarySwitch := nodeInfo{}
	binarySwitch.DeviceClass.Generic = genericBinarySwitch
	b, ok := nc.classify(binarySwitch)
	require.True(t, ok)
	_, isSwitch := b.(switchBuilder)
	assert.True(t, isSwitch)

	dimmer := nodeInfo{}
	dimmer.DeviceClass.Generic = genericMultilevelSwitch
	b, ok = nc.classify(dimmer)
	require.True(t, ok)
	_, isDimmer := b.(dimmerBuilder)
	assert.True(t, isDimmer)

	rgbBulb := nodeInfo{Values: map[string]nodeValue{"c": nv(ccColorSwitch, 0, "currentColor", "2", 255)}}
	rgbBulb.DeviceClass.Generic = genericMultilevelSwitch
	b, ok = nc.classify(rgbBulb)
	require.True(t, ok)
	_, isRGB := b.(rgbLightBuilder)
	assert.True(t, isRGB)

	sensor := nodeInfo{}
	sensor.DeviceClass.Generic = genericSensorNotification
	b, ok = nc.classify(sensor)
	require.True(t, ok)
	_, isSensor := b.(sensorBuilder)
	assert.True(t, isSensor)

	// A real AEON MultiSensor 6, recon'd against a live zwave-js-ui broker, reports generic class
	// 0x21 (Multilevel Sensor) rather than 0x07 (Sensor Notification) - see network.go's generic
	// class constants doc comment. Confirming this routes to sensorBuilder too is a regression
	// test for that finding, not a hypothetical.
	multilevelSensor := nodeInfo{}
	multilevelSensor.DeviceClass.Generic = genericMultilevelSensor
	b, ok = nc.classify(multilevelSensor)
	require.True(t, ok, "a real AEON MultiSensor 6's actual generic class (0x21) must still classify as a sensor")
	_, isSensor2 := b.(sensorBuilder)
	assert.True(t, isSensor2)

	binarySensor := nodeInfo{}
	binarySensor.DeviceClass.Generic = genericBinarySensor
	b, ok = nc.classify(binarySensor)
	require.True(t, ok)
	_, isSensor3 := b.(sensorBuilder)
	assert.True(t, isSensor3)

	unsupported := nodeInfo{}
	unsupported.DeviceClass.Generic = 0x08 // Thermostat - explicitly out of scope
	_, ok = nc.classify(unsupported)
	assert.False(t, ok)
}

func TestBuildNode_AutoAssignedID(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zwaveConfig{})

	n := nodeInfo{ID: 7, Available: true, Status: "Alive"}
	n.DeviceClass.Generic = genericBinarySwitch

	nc.buildNode(n)

	bd, ok := nc.devices["zwave-7"]
	require.True(t, ok)
	assert.True(t, bd.device.Address.IsReachable)
	assert.Equal(t, 7, bd.nodeID)
}

func TestBuildNode_IDOverride(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zwaveConfig{Devices: []deviceOverride{
		{DeviceID: "1-2-3", ID: "kitchen-plug"},
	}})

	n := nodeInfo{ID: 7, DeviceID: "1-2-3", Available: true, Status: "Alive"}
	n.DeviceClass.Generic = genericBinarySwitch

	nc.buildNode(n)

	_, ok := nc.devices["kitchen-plug"]
	assert.True(t, ok)
	_, ok = nc.devices["zwave-7"]
	assert.False(t, ok)
}

func TestBuildNode_IDCollisionAcrossNodes_Rejected(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zwaveConfig{Devices: []deviceOverride{
		{DeviceID: "1-2-3", ID: "outdoor-plug"},
	}})

	first := nodeInfo{ID: 7, DeviceID: "1-2-3", Available: true, Status: "Alive"}
	first.DeviceClass.Generic = genericBinarySwitch
	nc.buildNode(first)

	second := nodeInfo{ID: 9, DeviceID: "1-2-3", Available: true, Status: "Alive"}
	second.DeviceClass.Generic = genericBinarySwitch
	nc.buildNode(second)

	bd, ok := nc.devices["outdoor-plug"]
	require.True(t, ok)
	assert.Equal(t, 7, bd.nodeID, "the second node sharing the same id override must be rejected, not overwrite the first")
}

func TestBuildNode_DeadNode_Unreachable(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zwaveConfig{})

	n := nodeInfo{ID: 7, Available: false, Status: "Dead"}
	n.DeviceClass.Generic = genericBinarySwitch

	nc.buildNode(n)

	bd, ok := nc.devices["zwave-7"]
	require.True(t, ok)
	assert.False(t, bd.device.Address.IsReachable)
}

func TestBuildNode_UnsupportedGenericClass_Skipped(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zwaveConfig{})

	n := nodeInfo{ID: 7}
	n.DeviceClass.Generic = 0x08 // Thermostat

	nc.buildNode(n)

	assert.Empty(t, nc.devices)
}

func TestBuildNode_RGBWithoutOverride_Skipped(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zwaveConfig{})

	n := nodeInfo{ID: 7, Values: map[string]nodeValue{"c": nv(ccColorSwitch, 0, "currentColor", "2", 255)}}
	n.DeviceClass.Generic = genericMultilevelSwitch

	nc.buildNode(n)

	assert.Empty(t, nc.devices, "an RGB bulb with no colour_channels override should fail to build, not register with the wrong channel mapping")
}

func TestOnMessage_RoutesValueUpdate(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zwaveConfig{MQTT: mqttConfig{Prefix: "zwave"}})

	n := nodeInfo{ID: 7, Available: true, Status: "Alive"}
	n.DeviceClass.Generic = genericBinarySwitch
	nc.buildNode(n)

	v := valueID{nodeID: 7, topicBase: nodeTopicBase(n), commandClass: ccBinarySwitch, endpoint: 0, property: "targetValue"}
	nc.onMessage(nc.mqtt.topicFor(v), []byte("true"))

	bd := nc.devices["zwave-7"]
	assert.True(t, bd.device.GetGeneric().OnOff.State.IsOn)
}

// TestOnMessage_RoutesValueUpdate_RealNamedTopic confirms routing works against the exact topic
// shape a real zwave-js-ui broker publishes for a located, named node (not just the bare
// nodeID_<n> fallback the other tests use for brevity) - see mqttConn.topicFor's doc comment.
func TestOnMessage_RoutesValueUpdate_RealNamedTopic(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zwaveConfig{MQTT: mqttConfig{Prefix: "zwave"}})

	n := nodeInfo{ID: 2, Name: "motion_sensor", Loc: "second_floor/guest_bedroom", Available: true, Status: "Alive"}
	n.DeviceClass.Generic = genericMultilevelSensor
	nc.buildNode(n)

	nc.onMessage("zwave/second_floor/guest_bedroom/motion_sensor/notification/endpoint_0/Home_Security/Motion_sensor_status", []byte("8"))

	bd := nc.devices["zwave-2"]
	assert.True(t, bd.device.GetSensor().Presence.State.MotionDetected)
}

func TestOnMessage_RoutesStatusUpdate(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zwaveConfig{MQTT: mqttConfig{Prefix: "zwave"}})

	n := nodeInfo{ID: 7, Available: true, Status: "Alive"}
	n.DeviceClass.Generic = genericBinarySwitch
	nc.buildNode(n)

	// Real zwave-js-ui status payloads carry the node id explicitly ("nodeId") rather than the
	// bridge recovering it from the topic path - see parseStatus's doc comment.
	nc.onMessage("zwave/nodeID_7/status", []byte(`{"status":"Dead","nodeId":7}`))
	bd := nc.devices["zwave-7"]
	assert.False(t, bd.device.Address.IsReachable)

	nc.onMessage("zwave/nodeID_7/status", []byte(`{"status":"Alive","nodeId":7}`))
	assert.True(t, bd.device.Address.IsReachable)
}

func TestNodeHasCC(t *testing.T) {
	n := nodeInfo{Values: map[string]nodeValue{"c": nv(ccColorSwitch, 0, "currentColor", "2", 255)}}
	assert.True(t, nodeHasCC(n, ccColorSwitch))
	assert.False(t, nodeHasCC(n, ccBattery))
}

// TestApplyCommand_ThroughNetworkConn_NoDeadlock is a permanent regression test for a real
// deadlock: networkConn.applyCommand used to hold a lock across the blocking WriteValue call
// while WriteValue's own echo delivery re-entered that same lock via onMessage. Every other
// command test in this package calls a builder's applyCommand directly against a bare mqttConn,
// bypassing networkConn (and the lock) entirely - that's exactly why the original bug shipped
// undetected. This test goes through the real ZwaveBridge.ProcessCommand -> networkConn.
// applyCommand path so a regression here fails fast (via the internal timeout below) instead of
// hanging the whole test binary.
func TestApplyCommand_ThroughNetworkConn_NoDeadlock(t *testing.T) {
	nc, fc := newTestNetworkConn(t, zwaveConfig{MQTT: mqttConfig{Prefix: "zwave"}})

	n := nodeInfo{ID: 7, Available: true, Status: "Alive"}
	n.DeviceClass.Generic = genericBinarySwitch
	nc.buildNode(n)

	topic := nc.mqtt.topicFor(valueID{nodeID: 7, topicBase: nodeTopicBase(n), commandClass: ccBinarySwitch, endpoint: 0, property: "targetValue"})
	fc.respond(topic+"/set", topic, []byte("true"))

	done := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), &command.Command{
			DeviceId: "zwave-7",
			Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
		})
		done <- err
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("networkConn.applyCommand deadlocked - a WriteValue echo re-entering a per-device lock regressed")
	}

	bd := nc.devices["zwave-7"]
	assert.True(t, bd.device.GetGeneric().OnOff.State.IsOn)
}

// TestPerDeviceLocking_CommandOnOneDeviceDoesNotBlockAnother confirms the other half of the same
// fix: an in-flight command against one device must not stall state-update routing for a
// different, unrelated device. An earlier version of this bridge shared a single lock across the
// whole network for exactly this path.
func TestPerDeviceLocking_CommandOnOneDeviceDoesNotBlockAnother(t *testing.T) {
	orig := writeTimeout
	writeTimeout = 300 * time.Millisecond
	defer func() { writeTimeout = orig }()

	nc, _ := newTestNetworkConn(t, zwaveConfig{MQTT: mqttConfig{Prefix: "zwave"}})

	a := nodeInfo{ID: 7, Available: true, Status: "Alive"}
	a.DeviceClass.Generic = genericBinarySwitch
	nc.buildNode(a)

	b := nodeInfo{ID: 8, Available: true, Status: "Alive"}
	b.DeviceClass.Generic = genericBinarySwitch
	nc.buildNode(b)

	// No response armed for device A's write, so applyCommand blocks (holding only bd(A).mu) for
	// the full shrunk writeTimeout before giving up. Joined via the deferred Wait below, which -
	// because defers run LIFO - completes before the writeTimeout restore above: without this, the
	// test returns (and restores the shared package-level writeTimeout var) while this goroutine is
	// still reading it inside WriteValue's timeout select, a real data race caught by `go test
	// -race` (this goroutine wasn't joined at all in an earlier version of this test).
	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		_, _ = nc.applyCommand(context.Background(), &command.Command{
			DeviceId: "zwave-7",
			Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
		})
	}()
	defer func() {
		select {
		case <-aDone:
		case <-time.After(2 * time.Second):
			t.Fatal("device A's command goroutine never finished")
		}
	}()
	time.Sleep(20 * time.Millisecond) // let the goroutine above actually acquire bd(A).mu

	bTopic := nc.mqtt.topicFor(valueID{nodeID: 8, topicBase: nodeTopicBase(b), commandClass: ccBinarySwitch, endpoint: 0, property: "targetValue"})
	done := make(chan struct{})
	go func() {
		nc.onMessage(bTopic, []byte("true"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("device B's state update was blocked by device A's in-flight command - per-device locking regressed")
	}

	bd := nc.devices["zwave-8"]
	assert.True(t, bd.device.GetGeneric().OnOff.State.IsOn)
}

// TestReconnect_RediscoversNodesAndPreservesLastNonZeroLevel drives an actual reconnect - a second
// mqttConn.handleConnect call, the same trigger paho invokes on every real reconnect - rather than
// a second direct buildNode call, since prior test coverage never exercised the reconnect path
// through the mqtt layer at all (see the fakeMQTTClient's own doc comment on why that path is
// otherwise untested). It's also a regression test for buildNode carrying bd.lastNonZeroLevel
// forward when rebuilding the *same* node: an earlier version of this bridge unconditionally
// replaced a node's *builtDevice on every rediscovery pass, so even a reconnect of a still-known
// node reset lastNonZeroLevel back to zero - a plain "on" issued right after a reconnect would
// always jump to full brightness instead of restoring the level the light was actually at before
// the drop.
func TestReconnect_RediscoversNodesAndPreservesLastNonZeroLevel(t *testing.T) {
	nc, fc := newTestNetworkConn(t, zwaveConfig{MQTT: mqttConfig{Prefix: "zwave"}})

	n := nodeInfo{ID: 3, Available: true, Status: "Alive", Values: map[string]nodeValue{"v": nv(ccMultilevelSwitch, 0, "targetValue", "", 50)}}
	n.DeviceClass.Generic = genericMultilevelSwitch
	nc.buildNode(n)

	levelTopic := nc.mqtt.topicFor(valueID{nodeID: 3, topicBase: nodeTopicBase(n), commandClass: ccMultilevelSwitch, endpoint: 0, property: "targetValue"})

	// Turn the light off; lastNonZeroLevel should remember 50 from the initial build's cached state.
	fc.respond(levelTopic+"/set", levelTopic, []byte("0"))
	_, err := nc.applyCommand(context.Background(), &command.Command{DeviceId: "zwave-3", Details: &command.Command_OnOff{OnOff: &command.OnOff{On: false}}})
	require.NoError(t, err)

	// Simulate a broker reconnect: arm a fresh getNodes response (still reporting the node off, so
	// the fresh snapshot alone gives buildNode nothing to recover lastNonZeroLevel from) and invoke
	// handleConnect directly, exactly as paho's SetOnConnectHandler would on a real reconnect.
	offNode := nodeInfo{ID: 3, Available: true, Status: "Alive", Values: map[string]nodeValue{"v": nv(ccMultilevelSwitch, 0, "targetValue", "", 0)}}
	offNode.DeviceClass.Generic = genericMultilevelSwitch
	apiTopic := nc.mqtt.apiResponseTopic("getNodes")
	fc.respond(apiTopic+"/set", apiTopic, mustJSON(apiResponse{Success: true, Result: mustJSON([]nodeInfo{offNode})}))
	nc.mqtt.handleConnect(fc)

	require.Contains(t, nc.devices, "zwave-3", "reconnect's rediscovery pass must still find the node")

	// A plain "on" after the reconnect should still restore 50%, not jump to full brightness.
	fc.respond(levelTopic+"/set", levelTopic, mustJSON(50))
	d, err := nc.applyCommand(context.Background(), &command.Command{DeviceId: "zwave-3", Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}})
	require.NoError(t, err)
	assert.Equal(t, zwaveLevelToPercent(50), d.GetLight().Brightness.State.Level, "lastNonZeroLevel must survive a reconnect's rediscovery pass")
}

func TestReconnect_PrunesNodeMissingFromFreshGetNodes(t *testing.T) {
	nc, fc := newTestNetworkConn(t, zwaveConfig{MQTT: mqttConfig{Prefix: "zwave"}})

	staying := nodeInfo{ID: 3, Available: true, Status: "Alive"}
	staying.DeviceClass.Generic = genericBinarySwitch
	nc.buildNode(staying)

	leaving := nodeInfo{ID: 7, Available: true, Status: "Alive"}
	leaving.DeviceClass.Generic = genericBinarySwitch
	nc.buildNode(leaving)

	require.Contains(t, nc.devices, "zwave-3")
	require.Contains(t, nc.devices, "zwave-7")
	require.Contains(t, nc.zb.deviceOwner, "zwave-7")

	// Simulate a broker reconnect where node 7 has been excluded/factory-reset off the network:
	// the fresh getNodes response only reports node 3.
	apiTopic := nc.mqtt.apiResponseTopic("getNodes")
	fc.respond(apiTopic+"/set", apiTopic, mustJSON(apiResponse{Success: true, Result: mustJSON([]nodeInfo{staying})}))
	nc.mqtt.handleConnect(fc)

	assert.Contains(t, nc.devices, "zwave-3", "the node still present in getNodes must survive")
	assert.NotContains(t, nc.devices, "zwave-7", "the node missing from getNodes must be pruned")
	assert.NotContains(t, nc.nodeToDeviceID, 7)
	assert.NotContains(t, nc.zb.deviceOwner, "zwave-7", "ProcessCommand routing must be pruned too, not just the device map")

	for topic, id := range nc.topicToDeviceID {
		assert.NotEqual(t, "zwave-7", id, "topic %q still routes to a pruned device", topic)
	}

	_, err := nc.applyCommand(context.Background(), &command.Command{DeviceId: "zwave-7", Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}})
	assert.ErrorIs(t, err, bridge.ErrDeviceNotFound, "a command against the pruned device id must fail fast, not hang or silently succeed")
}

func TestApplyCommand_RestoresLastNonZeroLevelOnPlainOn(t *testing.T) {
	nc, fc := newTestNetworkConn(t, zwaveConfig{MQTT: mqttConfig{Prefix: "zwave"}})

	n := nodeInfo{ID: 3, Values: map[string]nodeValue{"v": nv(ccMultilevelSwitch, 0, "targetValue", "", 50)}}
	n.DeviceClass.Generic = genericMultilevelSwitch
	nc.buildNode(n)

	levelTopic := nc.mqtt.topicFor(valueID{nodeID: 3, topicBase: nodeTopicBase(n), commandClass: ccMultilevelSwitch, endpoint: 0, property: "targetValue"})

	fc.respond(levelTopic+"/set", levelTopic, []byte("0"))
	_, err := nc.applyCommand(context.Background(), &command.Command{DeviceId: "zwave-3", Details: &command.Command_OnOff{OnOff: &command.OnOff{On: false}}})
	require.NoError(t, err)

	fc.respond(levelTopic+"/set", levelTopic, mustJSON(50))
	d, err := nc.applyCommand(context.Background(), &command.Command{DeviceId: "zwave-3", Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}})
	require.NoError(t, err)
	assert.Equal(t, zwaveLevelToPercent(50), d.GetLight().Brightness.State.Level)
	assert.NotEqual(t, zwaveLevelToPercent(dimmerFullOnLevel), d.GetLight().Brightness.State.Level)
}

func TestApplyCommand_PlainOnWithNoTrackedLevel_FallsBackToFullBrightness(t *testing.T) {
	nc, fc := newTestNetworkConn(t, zwaveConfig{MQTT: mqttConfig{Prefix: "zwave"}})

	n := nodeInfo{ID: 3} // never reported/commanded above zero
	n.DeviceClass.Generic = genericMultilevelSwitch
	nc.buildNode(n)

	levelTopic := nc.mqtt.topicFor(valueID{nodeID: 3, topicBase: nodeTopicBase(n), commandClass: ccMultilevelSwitch, endpoint: 0, property: "targetValue"})
	fc.respond(levelTopic+"/set", levelTopic, mustJSON(dimmerFullOnLevel))

	d, err := nc.applyCommand(context.Background(), &command.Command{DeviceId: "zwave-3", Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}})
	require.NoError(t, err)
	assert.Equal(t, zwaveLevelToPercent(dimmerFullOnLevel), d.GetLight().Brightness.State.Level)
}

// TestBuildNode_SetsNonEmptyVersion and TestApplyCommand_UpdatesVersion confirm this bridge
// actually populates Device.version (via computeVersion) rather than leaving it permanently
// empty, which would silently disable service/bridge/api.go's checkVersion staleness check for
// every zwave device regardless of what a client supplies as Command.version.
func TestBuildNode_SetsNonEmptyVersion(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zwaveConfig{})

	n := nodeInfo{ID: 7, Available: true, Status: "Alive"}
	n.DeviceClass.Generic = genericBinarySwitch
	nc.buildNode(n)

	bd := nc.devices["zwave-7"]
	assert.NotEmpty(t, bd.device.Version)
}

func TestApplyCommand_UpdatesVersion(t *testing.T) {
	nc, fc := newTestNetworkConn(t, zwaveConfig{MQTT: mqttConfig{Prefix: "zwave"}})

	n := nodeInfo{ID: 7, Available: true, Status: "Alive"}
	n.DeviceClass.Generic = genericBinarySwitch
	nc.buildNode(n)
	versionBefore := nc.devices["zwave-7"].device.Version

	topic := nc.mqtt.topicFor(valueID{nodeID: 7, topicBase: nodeTopicBase(n), commandClass: ccBinarySwitch, endpoint: 0, property: "targetValue"})
	fc.respond(topic+"/set", topic, []byte("true"))

	d, err := nc.applyCommand(context.Background(), &command.Command{
		DeviceId: "zwave-7",
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	require.NoError(t, err)
	assert.NotEqual(t, versionBefore, d.Version, "a state-changing command must move the device's version")
}

func TestParseStatus(t *testing.T) {
	nodeID, reachable, ok := parseStatus([]byte(`{"status":"Dead","nodeId":7}`))
	assert.True(t, ok)
	assert.Equal(t, 7, nodeID)
	assert.False(t, reachable)

	nodeID, reachable, ok = parseStatus([]byte(`{"status":"Alive","nodeId":2}`))
	assert.True(t, ok)
	assert.Equal(t, 2, nodeID)
	assert.True(t, reachable)

	_, _, ok = parseStatus([]byte(`not json`))
	assert.False(t, ok)

	// No "status" field at all (e.g. the bare-boolean shape this bridge doesn't have a node id
	// for) is treated as unparseable, same as malformed JSON - see parseStatus's doc comment.
	_, _, ok = parseStatus([]byte(`true`))
	assert.False(t, ok)
}
