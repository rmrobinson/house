package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

// Generic Z-Wave device classes this bridge knows how to build a house device from. genericBinarySwitch
// and genericMultilevelSwitch are confirmed against real hardware on a live zwave-js-ui broker (a Zooz
// binary switch/plug). genericSensorNotification (0x07) is the generic class the Z-Wave specification
// documents for this device category, but the real AEON MultiSensor 6 recon'd against that same broker
// actually reports genericMultilevelSensor (0x21) - both are routed to sensorBuilder below, along with
// genericBinarySensor (0x20), since a real multisensor's primary generic class evidently depends on
// which of its command classes the device considers "primary", not strictly the Notification-CC-implies-
// Sensor-Notification-class assumption an earlier version of this bridge made.
const (
	genericBinarySwitch       = 0x10
	genericMultilevelSwitch   = 0x11
	genericBinarySensor       = 0x20
	genericMultilevelSensor   = 0x21
	genericSensorNotification = 0x07
)

// Command classes this bridge parses. Values confirmed against the design doc's real-hardware
// recon (a real AEON MultiSensor 6 and a real Zooz binary switch/plug) for 37/38/49/113/128; 51
// is well documented but its propertyKey layout is model-specific - see rgbLightBuilder.
const (
	ccBinarySwitch     = 37
	ccMultilevelSwitch = 38
	ccColorSwitch      = 51
	ccMultilevelSensor = 49
	ccNotification     = 113
	ccBinarySensor     = 48
	ccBattery          = 128
)

// nodeInfo mirrors the subset of a zwave-js-ui getNodes response entry this bridge consumes.
type nodeInfo struct {
	ID        int    `json:"id"`
	Status    string `json:"status"`
	Ready     bool   `json:"ready"`
	Available bool   `json:"available"`
	Name      string `json:"name"`
	Loc       string `json:"loc"`

	// DeviceID is "<manufacturerId>-<productId>-<producttype>" - stable per physical product
	// model, used as the key for this bridge's config overrides.
	DeviceID string `json:"deviceId"`

	DeviceClass struct {
		Basic    int `json:"basic"`
		Generic  int `json:"generic"`
		Specific int `json:"specific"`
	} `json:"deviceClass"`

	DeviceConfig struct {
		Description string `json:"description"`
		Label       string `json:"label"`
	} `json:"deviceConfig"`

	// Values holds this node's cached value list, keyed by zwave-js-ui's own value-id string
	// (opaque to this bridge - we only ever range over it). Empty for a dead or
	// never-interviewed node even though DeviceClass is always populated - see
	// networkConn.classify's doc comment for why DeviceClass, not this map, drives which house
	// device type a node becomes.
	Values map[string]nodeValue `json:"values"`
}

// nodeValue mirrors one entry of a node's cached value list from getNodes. Its Property/
// PropertyKey fields are still plain Go strings - see UnmarshalJSON - so every other file in this
// bridge keeps treating them as such; only the JSON decoding itself needs to know they aren't
// always wire-encoded as strings.
type nodeValue struct {
	CommandClass int             `json:"commandClass"`
	Endpoint     int             `json:"endpoint"`
	Property     string          `json:"-"`
	PropertyKey  string          `json:"-"`
	Value        json.RawMessage `json:"value"`
	// States lists a Notification CC value's device-defined event codes/labels, e.g.
	// [{"text":"idle","value":0},{"text":"Motion detection","value":8}] - confirmed against a real
	// AEON MultiSensor 6's getNodes response. An earlier version of this bridge assumed this was a
	// {"<numeric code as string>":"<label>"} object; decoding it as that against a real broker
	// failed outright (a JSON array doesn't unmarshal into a map) and aborted the entire discovery
	// pass, not just this one node - every device on the bridge silently stopped updating.
	States []nodeValueState `json:"states"`
}

// UnmarshalJSON decodes Property/PropertyKey leniently: zwave-js-ui's getNodes response encodes
// them as a JSON string for most command classes (e.g. Notification CC's "Home Security"), but as
// a bare JSON number for others - confirmed against a real broker, where every Configuration CC
// (112) value's property is its raw numeric parameter index, not a quoted string. A strict
// `string`-typed field decode fails outright the instant it hits one of these, which - since
// Configuration CC values are included in every node's cached value list regardless of whether
// this bridge does anything with CC 112 - aborted getNodes entirely, for every node, not just a
// CC 112 value this bridge never reads.
func (v *nodeValue) UnmarshalJSON(b []byte) error {
	type alias nodeValue
	var raw struct {
		alias
		Property    json.RawMessage `json:"property"`
		PropertyKey json.RawMessage `json:"propertyKey"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*v = nodeValue(raw.alias)
	v.Property = decodeFlexString(raw.Property)
	v.PropertyKey = decodeFlexString(raw.PropertyKey)
	return nil
}

// decodeFlexString decodes a JSON value that may be a string, a number, or absent into a
// canonical Go string - see nodeValue.UnmarshalJSON.
func decodeFlexString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// nodeValueState is one entry of nodeValue.States. Value is left as raw JSON rather than decoded
// to a concrete Go type since the states list is reused across CCs whose "value" is sometimes a
// number (Notification CC's event codes) and sometimes a bool - see valuesEqual, which already
// normalizes exactly this comparison for WriteValue's echo check.
type nodeValueState struct {
	Text  string          `json:"text"`
	Value json.RawMessage `json:"value"`
}

func (v nodeValue) id(n nodeInfo) valueID {
	return valueID{nodeID: n.ID, topicBase: nodeTopicBase(n), commandClass: v.CommandClass, endpoint: v.Endpoint, property: v.Property, propertyKey: v.PropertyKey}
}

// nodeHasCC reports whether n's cached value list includes any value for the given command
// class. Used to detect a Color Switch (CC 51) alongside a Multilevel Switch generic class,
// which distinguishes an RGB(W) bulb from a plain dimmer - see the design doc's device-class
// mapping table.
func nodeHasCC(n nodeInfo, cc int) bool {
	for _, v := range n.Values {
		if v.CommandClass == cc {
			return true
		}
	}
	return false
}

// builtDevice tracks everything needed to keep a single house device driven by a Z-Wave node in
// sync: the device itself, the builder that produced it, the role->valueId table applyState/
// applyCommand route through, and the node it came from (for status-topic routing).
//
// mu guards device (and lastNonZeroLevel) and is held across both applyState and applyCommand -
// including applyCommand's blocking WriteValue call(s) - for this device only. Scoping the lock
// per-device rather than sharing one lock across the whole network (as an earlier version of this
// file did) means a slow or stuck command against one device no longer stalls state-update
// processing for every other device on the bridge; see mqttConn.dispatch's doc comment for why
// this is also deadlock-safe against the command's own echo arriving mid-call.
type builtDevice struct {
	mu      sync.Mutex
	device  *device.Device
	builder deviceBuilder
	roles   map[string]valueID
	nodeID  int

	// lastNonZeroLevel is the most recent nonzero Brightness percent reported or commanded for a
	// Light device (dimmer/RGB bulb), tracked so a plain OnOff-on command can restore it - see
	// resolveRestoreOnLevel - instead of always jumping to full brightness. Left at zero (meaning
	// "nothing to restore yet") for device types with no Brightness trait.
	lastNonZeroLevel int32
}

// networkConn owns Z-Wave node discovery (via getNodes), value-topic routing, and the device
// registry built from it. Unlike bridges/esphome's nodeConn (one connection per physical node),
// there is exactly one networkConn per bridge process, sharing the single mqttConn for the whole
// Z-Wave network - see the design doc's "Architecture" section for why.
type networkConn struct {
	logger *zap.Logger
	svc    *bridge.Service
	zb     *ZwaveBridge
	mqtt   *mqttConn
	cfg    zwaveConfig

	// connectMu serializes onConnect end-to-end, mirroring bridges/esphome/node.go's connectMu:
	// paho invokes the connect callback on every successful (re)connect with no built-in
	// de-duplication, so a rapidly flapping connection could otherwise start a second discovery
	// pass (getNodes can take up to getNodesTimeout) while the first is still running.
	connectMu sync.Mutex

	// mu guards the routing tables below (and devices' membership, i.e. adding/looking up a
	// *builtDevice) - never held across a blocking network call. Each builtDevice has its own
	// lock for that - see its doc comment.
	mu sync.Mutex
	// topicToDeviceID/topicToRole route an incoming value-topic message to the house device and
	// role it belongs to. Keyed by the exact wire topic string (precomputed once per role at
	// buildNode time via mqttConn.topicFor) rather than by parsing the incoming topic back into a
	// valueID: zwave-js-ui's named-topics scheme intermixes a per-node location/name segment with
	// a command-class name and an underscore-slugified property name, none of which can be
	// unambiguously split back apart from the topic string alone (a location can itself contain
	// "/", and a slugified property name can't be distinguished from a multi-word command-class
	// name by structure). Since every topic this bridge cares about is already known in full at
	// build time, exact string matching sidesteps that ambiguity entirely instead of trying to
	// parse it away.
	topicToDeviceID map[string]string
	topicToRole     map[string]string
	nodeToDeviceID  map[int]string
	devices         map[string]*builtDevice
}

func newNetworkConn(logger *zap.Logger, svc *bridge.Service, zb *ZwaveBridge, mc *mqttConn, cfg zwaveConfig) *networkConn {
	nc := &networkConn{
		logger:          logger,
		svc:             svc,
		zb:              zb,
		mqtt:            mc,
		cfg:             cfg,
		topicToDeviceID: make(map[string]string),
		topicToRole:     make(map[string]string),
		nodeToDeviceID:  make(map[int]string),
		devices:         make(map[string]*builtDevice),
	}
	mc.onMessage = nc.onMessage
	mc.onConnect = nc.onConnect
	return nc
}

// onConnect runs discovery. It fires on every (re)connect (paho invokes the connect handler this
// is wired from on every successful reconnect, not just the first), which keeps a bridge that
// dropped its broker connection for a while catching up on nodes it missed rather than only ever
// discovering once for the process lifetime.
func (nc *networkConn) onConnect() {
	nc.connectMu.Lock()
	defer nc.connectMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), getNodesTimeout+writeTimeout)
	defer cancel()

	nodes, err := nc.mqtt.getNodes(ctx)
	if err != nil {
		nc.logger.Error("unable to discover nodes", zap.Error(err))
		return
	}

	seen := make(map[int]bool, len(nodes))
	for _, n := range nodes {
		seen[n.ID] = true
		nc.buildNode(n)
	}
	nc.pruneRemovedNodes(seen)
}

// pruneRemovedNodes removes every device whose node did not appear in the getNodes response this
// onConnect pass just processed - a node that's been excluded, factory-reset, or replaced on the
// Z-Wave network otherwise stays a permanent "ghost" device forever: devices/topicToDeviceID/
// topicToRole/nodeToDeviceID would only ever grow across the process's lifetime as hardware
// churns, and a command routed to the ghost id would deterministically time out rather than fail
// fast. Mirrors bridges/roku/bridge.go's Refresh, which does the same "diff a fresh discovery pass
// against what's already known, RemoveDevice what's missing" pattern.
//
// seen is keyed by raw node ID (not house device id), populated from onConnect's fresh nodes
// slice before buildNode runs - so a node that fails to build this pass (an unsupported generic
// class was always true, or a transient override/build error) still counts as seen and is not
// pruned just because buildNode didn't register it. That avoids flapping a device out on a single
// bad discovery pass; it only prunes nodes genuinely absent from getNodes.
func (nc *networkConn) pruneRemovedNodes(seen map[int]bool) {
	nc.mu.Lock()
	var removedIDs []string
	for nodeID, id := range nc.nodeToDeviceID {
		if seen[nodeID] {
			continue
		}
		removedIDs = append(removedIDs, id)
		delete(nc.nodeToDeviceID, nodeID)
		delete(nc.devices, id)
		for topic, owner := range nc.topicToDeviceID {
			if owner == id {
				delete(nc.topicToDeviceID, topic)
				delete(nc.topicToRole, topic)
			}
		}
	}
	nc.mu.Unlock()

	for _, id := range removedIDs {
		nc.logger.Info("node no longer present in getNodes; removing its device", zap.String("device_id", id))
		nc.zb.unregisterDevice(id)
		nc.svc.RemoveDevice(id)
	}
}

// buildNode classifies a single discovered node, builds its house device (if its generic device
// class is one this bridge supports), and registers it. Nodes with an unsupported generic class
// are skipped with a log line rather than treated as an error - per the design doc, phase 1 is
// deliberately scoped to switches/dimmers/RGB bulbs/sensors, not the whole Z-Wave device class
// table.
func (nc *networkConn) buildNode(n nodeInfo) {
	builder, ok := nc.classify(n)
	if !ok {
		nc.logger.Debug("skipping node with unsupported generic device class",
			zap.Int("node_id", n.ID), zap.Int("generic_class", n.DeviceClass.Generic))
		return
	}

	override := nc.cfg.overrideFor(n.DeviceID)

	d, roles, err := builder.build(n, override)
	if err != nil {
		nc.logger.Error("unable to build device for node", zap.Int("node_id", n.ID), zap.Error(err))
		return
	}

	id := override.ID
	if id == "" {
		id = fmt.Sprintf("zwave-%d", n.ID)
	}
	d.Id = id
	if d.Address == nil {
		d.Address = &device.Device_Address{}
	}
	d.Address.IsReachable = n.Available && !strings.EqualFold(n.Status, "Dead")

	nc.mu.Lock()
	// A device id already claimed by a *different* node - almost always a config `id` override
	// applied to a device_id shared by more than one physical node - is refused rather than
	// silently overwritten: overwriting would leave the old node's roles in topicToDeviceID still
	// pointing at the new node's *builtDevice, cross-wiring the two nodes' state updates onto one
	// house device. The node this happened to simply doesn't get built until the collision is
	// fixed in config; every other node keeps working.
	existing, claimed := nc.devices[id]
	if claimed && existing.nodeID != n.ID {
		nc.mu.Unlock()
		nc.logger.Error("device id already claimed by a different node; skipping - check for a colliding id override in config",
			zap.String("device_id", id), zap.Int("node_id", n.ID), zap.Int("existing_node_id", existing.nodeID))
		return
	}

	bd := &builtDevice{device: d, builder: builder, roles: roles, nodeID: n.ID}
	if claimed {
		// The same node is being rebuilt on a reconnect's rediscovery pass (onConnect re-runs
		// getNodes on every reconnect, not just the first). getNodes' snapshot alone can't tell us
		// what this bridge process previously remembered as the device's last nonzero brightness -
		// that's process-local bookkeeping, not Z-Wave state - so it's carried over from the
		// *builtDevice being replaced rather than silently reset to zero. trackLastNonZeroLevel
		// below still refreshes it from d's freshly-read state if that's more current (i.e. the
		// node is reporting a nonzero level right now); this only preserves the old value for the
		// case getNodes' fresh read can't improve on: the node reporting zero/off at the exact
		// moment of reconnect despite having been at a nonzero level moments before.
		existing.mu.Lock()
		bd.lastNonZeroLevel = existing.lastNonZeroLevel
		existing.mu.Unlock()
	}
	trackLastNonZeroLevel(bd)
	nc.devices[id] = bd
	nc.nodeToDeviceID[n.ID] = id
	for role, v := range roles {
		topic := nc.mqtt.topicFor(v)
		if ownerID, claimed := nc.topicToDeviceID[topic]; claimed && ownerID != id {
			nc.logger.Warn("value claimed by more than one device; updates will only route to the most recently matched device",
				zap.Int("node_id", n.ID), zap.String("role", role), zap.String("previous_device_id", ownerID), zap.String("device_id", id))
		}
		nc.topicToDeviceID[topic] = id
		nc.topicToRole[topic] = role
	}
	nc.mu.Unlock()

	nc.zb.registerDevice(id, nc)
	d.Version = computeVersion(d)
	nc.svc.UpdateDevice(d)
}

// classify maps a node's generic device class to the deviceBuilder that owns it, per the design
// doc's device-class mapping table. A Multilevel Switch node that also reports a Color Switch
// (CC 51) value is an RGB(W) bulb rather than a plain dimmer; a dead/never-interviewed bulb whose
// getNodes response has an empty Values map is indistinguishable from a plain dimmer until it
// completes its first interview - same caveat as the sensor optional-trait detection below.
func (nc *networkConn) classify(n nodeInfo) (deviceBuilder, bool) {
	switch n.DeviceClass.Generic {
	case genericBinarySwitch:
		return switchBuilder{}, true
	case genericMultilevelSwitch:
		if nodeHasCC(n, ccColorSwitch) {
			return rgbLightBuilder{}, true
		}
		return dimmerBuilder{}, true
	case genericSensorNotification, genericMultilevelSensor, genericBinarySensor:
		return sensorBuilder{}, true
	default:
		return nil, false
	}
}

// onMessage routes a single incoming value or status message to the device/role it belongs to.
// Installed as mqttConn.onMessage by newNetworkConn. A status topic is identified structurally (it
// always ends in "/status" under zwave-js-ui's named-topics scheme, regardless of how many
// location segments precede it - unlike a value topic's node id, a status topic's node id isn't
// reliably recoverable from the topic string alone, so handleStatus reads it out of the payload's
// own "nodeId" field instead - confirmed against a real broker); everything else is looked up by
// exact topic string against topicToDeviceID/topicToRole (see their doc comment for why this
// bridge doesn't attempt to parse a value topic back into a valueID).
func (nc *networkConn) onMessage(topic string, payload []byte) {
	if strings.HasSuffix(topic, "/status") {
		nc.handleStatus(payload)
		return
	}

	nc.mu.Lock()
	deviceID, ok := nc.topicToDeviceID[topic]
	if !ok {
		nc.mu.Unlock()
		return
	}
	role := nc.topicToRole[topic]
	bd := nc.devices[deviceID]
	nc.mu.Unlock()

	bd.mu.Lock()
	defer bd.mu.Unlock()

	bd.builder.applyState(bd.device, role, payload)
	trackLastNonZeroLevel(bd)
	bd.device.Version = computeVersion(bd.device)
	nc.svc.UpdateDevice(bd.device)
}

// handleStatus applies a node status-topic update to the owning device's Address.IsReachable.
// The node id is read from the payload's own "nodeId" field (confirmed present on a real broker's
// status payloads, alongside "status":"Alive|Asleep|Dead") rather than the topic path, since a
// status topic's node-identifying segment isn't reliably separable from an arbitrary-length
// location prefix by structure alone.
func (nc *networkConn) handleStatus(payload []byte) {
	nodeID, reachable, ok := parseStatus(payload)
	if !ok {
		return
	}

	nc.mu.Lock()
	deviceID, ok := nc.nodeToDeviceID[nodeID]
	if !ok {
		nc.mu.Unlock()
		return
	}
	bd := nc.devices[deviceID]
	nc.mu.Unlock()

	bd.mu.Lock()
	defer bd.mu.Unlock()

	if bd.device.Address == nil {
		bd.device.Address = &device.Device_Address{}
	}
	bd.device.Address.IsReachable = reachable
	nc.svc.UpdateDevice(bd.device)
}

// parseStatus decodes a node status-topic payload. zwave-js-ui's named-topics scheme always
// publishes the object form with an explicit "nodeId" (confirmed against a real broker); the bare
// boolean form (no node id available at all) is kept as a fallback for robustness but can't
// resolve which device it's for on its own, so it's only useful if a caller already knows the node
// id some other way - onMessage doesn't, so in practice that shape is simply ignored today.
func parseStatus(payload []byte) (nodeID int, reachable bool, ok bool) {
	var s struct {
		Status string `json:"status"`
		NodeID int    `json:"nodeId"`
	}
	if err := json.Unmarshal(payload, &s); err == nil && s.Status != "" {
		return s.NodeID, strings.EqualFold(s.Status, "Alive"), true
	}
	return 0, false, false
}

// applyCommand routes a command to the device it targets and, on success, returns the
// optimistically-updated device - same contract as bridges/esphome/node.go's applyCommand. The
// returned device is a clone taken while still holding bd's lock, so the caller can't race with a
// concurrent state update mutating the same *device.Device.
func (nc *networkConn) applyCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	nc.mu.Lock()
	bd, ok := nc.devices[cmd.DeviceId]
	nc.mu.Unlock()
	if !ok {
		return nil, bridge.ErrDeviceNotFound
	}

	// Held for the duration of the write: applyCommand issues one or more WriteValue calls that
	// block on the node's echo. Scoped to this one device (see builtDevice.mu's doc comment) - a
	// concurrent command or state update against a *different* device proceeds independently.
	bd.mu.Lock()
	defer bd.mu.Unlock()

	cmd = resolveRestoreOnLevel(bd, cmd)

	if err := bd.builder.applyCommand(ctx, nc.mqtt, bd.roles, bd.device, cmd); err != nil {
		return nil, err
	}
	trackLastNonZeroLevel(bd)
	bd.device.Version = computeVersion(bd.device)
	return proto.Clone(bd.device).(*device.Device), nil
}

// trackLastNonZeroLevel updates bd.lastNonZeroLevel from bd.device's current Brightness state, if
// it has one and it's nonzero. Called after every applyState/applyCommand that might have changed
// it. The caller must already hold bd.mu (or, for a not-yet-published bd inside buildNode, be its
// only owner).
func trackLastNonZeroLevel(bd *builtDevice) {
	l := bd.device.GetLight()
	if l.GetBrightness() == nil {
		return
	}
	if lvl := l.Brightness.State.Level; lvl > 0 {
		bd.lastNonZeroLevel = lvl
	}
}

// resolveRestoreOnLevel rewrites a plain OnOff-on command against a Light device (dimmer or RGB
// bulb) that's currently fully off into an equivalent BrightnessAbsolute targeting the last known
// nonzero level, so turning it back on restores where it was rather than always jumping to full
// brightness (dimmerFullOnLevel). Every other command - including OnOff-on for a device with no
// tracked level yet, e.g. one this bridge process has never seen above zero - passes through
// unchanged; dimmerBuilder/rgbLightBuilder's own OnOff handling (dimmerFullOnLevel) still owns
// that fallback. The caller must already hold bd.mu.
func resolveRestoreOnLevel(bd *builtDevice, cmd *command.Command) *command.Command {
	if cmd.GetOnOff() == nil || !cmd.GetOnOff().On || bd.lastNonZeroLevel == 0 {
		return cmd
	}
	l := bd.device.GetLight()
	if l.GetBrightness() == nil || l.Brightness.State.Level > 0 {
		return cmd
	}
	return &command.Command{
		DeviceId: cmd.DeviceId,
		Id:       cmd.Id,
		Version:  cmd.Version,
		Details:  &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: bd.lastNonZeroLevel}},
	}
}
