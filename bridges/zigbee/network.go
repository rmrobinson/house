package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

// bridgeDevice mirrors one entry of zigbee2mqtt's retained <base_topic>/bridge/devices array -
// the analogue of bridges/zwave's getNodes response entry, except pushed rather than pulled (see
// networkConn.onMessage/handleBridgeDevices) and self-describing via Definition.Exposes rather
// than a fixed generic-device-class enum, since Zigbee has no equivalent of Z-Wave's generic
// device class taxonomy - zigbee-herdsman-converters resolves every device model's actual
// capabilities into this exposes list itself.
type bridgeDevice struct {
	IEEEAddress  string `json:"ieee_address"`
	FriendlyName string `json:"friendly_name"`
	// Type is "Coordinator", "Router", or "EndDevice". The coordinator entry (the zigbee2mqtt
	// adapter itself) is always present and never has anything this bridge can build a device
	// from - see networkConn.classify.
	Type string `json:"type"`
	// PowerSource is zigbee-herdsman-converters' own classification (e.g. "Battery", "Mains
	// (single phase)", "DC", "Unknown") - used directly for Sensor.Metadata.OnBattery rather than
	// inferred from which traits a device happens to expose, unlike bridges/zwave which has no
	// equivalent field to read.
	PowerSource string `json:"power_source"`
	Disabled    bool   `json:"disabled"`
	// Supported reports whether zigbee-herdsman-converters has a definition for this exact model
	// at all; an unsupported device's Definition is typically absent or has no Exposes, which
	// classify's empty-exposes check already handles, but checking this explicitly documents the
	// intent rather than relying on that as an implicit side effect.
	Supported  bool             `json:"supported"`
	Definition *deviceDefiniton `json:"definition"`
}

type deviceDefiniton struct {
	Model       string   `json:"model"`
	Vendor      string   `json:"vendor"`
	Description string   `json:"description"`
	Exposes     []expose `json:"exposes"`
}

// expose mirrors one entry of zigbee2mqtt's exposes API
// (https://www.zigbee2mqtt.io/guide/usage/exposes.html). A leaf expose (Type "binary"/"numeric"/
// "enum"/"text") describes one property directly; a composite expose (Type "light"/"switch"/
// "cover"/"lock"/"climate"/"fan", or the nested Type "composite" a light's colour capability
// uses) groups a set of leaf Features under it instead of carrying a Property itself. Endpoint is
// only set for a multi-endpoint device (e.g. a 2-gang wall switch); this bridge's phase 1 scope
// deliberately only reads the first matching top-level composite expose for classify, so a
// multi-gang device only gets its first endpoint modeled - see README's "Known limitations".
type expose struct {
	Type     string   `json:"type"`
	Name     string   `json:"name"`
	Property string   `json:"property"`
	Endpoint string   `json:"endpoint"`
	Unit     string   `json:"unit"`
	ValueMin *float64 `json:"value_min"`
	ValueMax *float64 `json:"value_max"`
	// ValueOn/ValueOff give the raw wire value a binary expose's property takes for its logical
	// true/false state. Confirmed, across the real devices this bridge was checked against, to
	// vary in JSON type per property on the very same physical device - a real IKEA STARKVIND air
	// purifier reports "led_enable" as a plain bool (true/false) but "child_lock" as a string
	// ("LOCK"/"UNLOCK") - so fanBuilder decodes and compares against these directly (see
	// decodeAny/fanToggle) rather than assuming every binary expose is a JSON bool.
	ValueOn  json.RawMessage `json:"value_on"`
	ValueOff json.RawMessage `json:"value_off"`
	// Values lists the allowed strings for an "enum"-typed leaf expose, e.g. a fan's "mode"
	// feature: ["off","auto","1",...,"9"] for a real IKEA STARKVIND.
	Values   []string `json:"values"`
	Features []expose `json:"features"`
}

// findFeature returns the leaf feature named name within a composite expose's Features list.
func findFeature(features []expose, name string) (expose, bool) {
	for _, f := range features {
		if f.Name == name {
			return f, true
		}
	}
	return expose{}, false
}

// findTopLevel returns the first top-level expose of the given Type from a device's Exposes list
// whose Endpoint is unset - see expose's doc comment on multi-endpoint scoping.
func findTopLevel(exposes []expose, typ string) (expose, bool) {
	for _, e := range exposes {
		if e.Type == typ && e.Endpoint == "" {
			return e, true
		}
	}
	return expose{}, false
}

// findByProperty returns the first top-level leaf expose (Type "binary"/"numeric") whose Property
// matches, with an unset Endpoint - used for the various optional sensor-capability exposes
// (contact/occupancy/temperature/humidity/... ) that zigbee2mqtt reports directly at the top
// level of a device's Exposes list, not nested under a composite.
func findByProperty(exposes []expose, property string) (expose, bool) {
	for _, e := range exposes {
		if e.Property == property && e.Endpoint == "" && (e.Type == "binary" || e.Type == "numeric") {
			return e, true
		}
	}
	return expose{}, false
}

// builtDevice tracks everything needed to keep a single house device driven by a zigbee2mqtt
// device in sync: the device itself, the builder that produced it, and the device's
// friendly_name (needed for both routing incoming state topics and addressing outgoing /set
// writes). mu guards device and is held across both applyState and applyCommand - including
// applyCommand's blocking WriteState call - for this device only, mirroring
// bridges/zwave/network.go's builtDevice.mu: a slow or stuck command against one device doesn't
// stall state-update processing for any other device.
type builtDevice struct {
	mu           sync.Mutex
	device       *device.Device
	builder      deviceBuilder
	friendlyName string
	ieeeAddress  string
}

// networkConn owns zigbee2mqtt device discovery (driven by the retained bridge/devices message),
// state-topic routing, and the device registry built from it. There is exactly one networkConn
// per bridge process, sharing the single mqttConn for the whole Zigbee network - same shape as
// bridges/zwave's networkConn, for the same reason (zigbee2mqtt, like zwave-js-ui, already
// multiplexes every device onto one broker connection, so there's no per-device dial step).
type networkConn struct {
	logger *zap.Logger
	svc    *bridge.Service
	zb     *ZigbeeBridge
	mqtt   *mqttConn
	cfg    zigbeeConfig

	// mu guards the maps below (and devices' membership) - never held across a blocking network
	// call. Each builtDevice has its own lock for that - see its doc comment.
	mu sync.Mutex
	// topicToDeviceID routes an incoming device-state topic (<base_topic>/<friendly_name>) to the
	// house device it belongs to - unlike bridges/zwave, there is exactly one topic per device
	// (zigbee2mqtt publishes one JSON blob with every known property, not one topic per
	// property), so there's no separate topicToRole table to maintain.
	topicToDeviceID map[string]string
	// friendlyNameToDeviceID additionally indexes by friendly_name alone (without the topic
	// prefix), used by handleAvailability, whose topic shape
	// (<base_topic>/<friendly_name>/availability) doesn't match topicToDeviceID's keys.
	friendlyNameToDeviceID map[string]string
	ieeeToDeviceID         map[string]string
	devices                map[string]*builtDevice
}

func newNetworkConn(logger *zap.Logger, svc *bridge.Service, zb *ZigbeeBridge, mc *mqttConn, cfg zigbeeConfig) *networkConn {
	nc := &networkConn{
		logger:                 logger,
		svc:                    svc,
		zb:                     zb,
		mqtt:                   mc,
		cfg:                    cfg,
		topicToDeviceID:        make(map[string]string),
		friendlyNameToDeviceID: make(map[string]string),
		ieeeToDeviceID:         make(map[string]string),
		devices:                make(map[string]*builtDevice),
	}
	mc.onMessage = nc.onMessage
	return nc
}

// onMessage routes a single incoming message to the right handler, by topic shape:
// bridge/devices (discovery), <friendly_name>/availability (reachability), or - everything else -
// a device's own full-state topic. Installed as mqttConn.onMessage by newNetworkConn. Any other
// <base_topic>/bridge/... topic (state, logging, info, event, groups, response/#, ...) is
// deliberately ignored: none of them identify a single device's state the way bridge/devices and
// a device's own topic do.
func (nc *networkConn) onMessage(topic string, payload []byte) {
	base := nc.mqtt.cfg.BaseTopic

	if topic == base+"/bridge/devices" {
		nc.handleBridgeDevices(payload)
		return
	}
	if strings.HasPrefix(topic, base+"/bridge/") {
		return
	}
	if strings.HasSuffix(topic, "/availability") {
		nc.handleAvailability(strings.TrimSuffix(strings.TrimPrefix(topic, base+"/"), "/availability"), payload)
		return
	}

	nc.mu.Lock()
	deviceID, ok := nc.topicToDeviceID[topic]
	if !ok {
		nc.mu.Unlock()
		return
	}
	bd := nc.devices[deviceID]
	nc.mu.Unlock()

	var state map[string]any
	if err := json.Unmarshal(payload, &state); err != nil {
		nc.logger.Warn("unable to decode device state", zap.String("topic", topic), zap.Error(err))
		return
	}

	bd.mu.Lock()
	defer bd.mu.Unlock()
	bd.builder.applyState(bd.device, state)
	bd.device.Version = computeVersion(bd.device)
	nc.svc.UpdateDevice(bd.device)
}

// handleBridgeDevices runs a full discovery/rebuild pass from a bridge/devices payload. Unlike
// bridges/zwave's onConnect (which only ever runs getNodes once per (re)connect), this fires
// every time the retained bridge/devices message arrives - which zigbee2mqtt republishes not
// just on every fresh subscribe (i.e. every reconnect, the zwave-equivalent case) but also live,
// any time a device is paired, removed, or renamed - so this bridge picks up network topology
// changes during normal operation too, not only after a reconnect.
func (nc *networkConn) handleBridgeDevices(payload []byte) {
	var devices []bridgeDevice
	if err := json.Unmarshal(payload, &devices); err != nil {
		nc.logger.Error("unable to decode bridge/devices", zap.Error(err))
		return
	}

	seen := make(map[string]bool, len(devices))
	for _, bd := range devices {
		if bd.IEEEAddress == "" {
			continue
		}
		seen[bd.IEEEAddress] = true
		nc.buildDevice(bd)
	}
	nc.pruneRemoved(seen)
}

// pruneRemoved removes every house device whose zigbee2mqtt device did not appear in the
// bridge/devices payload handleBridgeDevices just processed - a device that's been removed
// (zigbee2mqtt/bridge/request/device/remove, or factory-reset off the network) otherwise stays a
// permanent "ghost" device forever. Mirrors bridges/zwave/network.go's pruneRemovedNodes.
func (nc *networkConn) pruneRemoved(seen map[string]bool) {
	nc.mu.Lock()
	var removedIDs []string
	for ieee, id := range nc.ieeeToDeviceID {
		if seen[ieee] {
			continue
		}
		removedIDs = append(removedIDs, id)
		delete(nc.ieeeToDeviceID, ieee)
		delete(nc.devices, id)
		for topic, owner := range nc.topicToDeviceID {
			if owner == id {
				delete(nc.topicToDeviceID, topic)
			}
		}
		for fn, owner := range nc.friendlyNameToDeviceID {
			if owner == id {
				delete(nc.friendlyNameToDeviceID, fn)
			}
		}
	}
	nc.mu.Unlock()

	for _, id := range removedIDs {
		nc.logger.Info("device no longer present in bridge/devices; removing", zap.String("device_id", id))
		nc.zb.unregisterDevice(id)
		nc.svc.RemoveDevice(id)
	}
}

// buildDevice classifies a single discovered zigbee2mqtt device, builds its house device (if this
// bridge recognizes a usable shape in its exposes list), and registers it.
//
// An already-known device (same ieee_address) is deliberately NOT rebuilt - only a friendly_name
// rename is applied, by re-pointing the routing tables at the new state topic. bridge/devices
// carries a device's capabilities only, never its current property values (see
// deviceBuilder.build's doc comment), so rebuilding here would reset every trait this bridge has
// since learned via applyState back to its zero value - and, per handleBridgeDevices' doc
// comment, bridge/devices is republished live any time *any* device on the network pairs, is
// removed, or is renamed, not only for the device that actually changed. An earlier version of
// this bridge rebuilt unconditionally on every pass, which meant pairing one new device would
// reset every other device's live state on the same network back to zero. The trade-off accepted
// here: a device's capabilities (its exposes) are assumed stable for as long as this bridge
// process runs, so a firmware update that changes them mid-run won't be picked up without a
// bridge restart - a much rarer event than routine pair/rename/remove activity elsewhere on the
// network.
func (nc *networkConn) buildDevice(bd bridgeDevice) {
	nc.mu.Lock()
	id, known := nc.ieeeToDeviceID[bd.IEEEAddress]
	var existingBD *builtDevice
	if known {
		existingBD = nc.devices[id]
	}
	nc.mu.Unlock()

	if known {
		// existingBD.friendlyName is mutated under existingBD.mu, not nc.mu - applyCommand reads
		// it while holding the same lock (see its doc comment), and this codebase's convention is
		// to never hold nc.mu and a *builtDevice's own mu at the same time (every other access
		// pattern in this file releases one before acquiring the other), so this is done as two
		// separate critical sections rather than nesting the locks here.
		existingBD.mu.Lock()
		oldFriendlyName := existingBD.friendlyName
		renamed := oldFriendlyName != bd.FriendlyName
		if renamed {
			existingBD.friendlyName = bd.FriendlyName
		}
		existingBD.mu.Unlock()

		if renamed {
			nc.mu.Lock()
			delete(nc.topicToDeviceID, nc.mqtt.stateTopic(oldFriendlyName))
			delete(nc.friendlyNameToDeviceID, oldFriendlyName)
			nc.topicToDeviceID[nc.mqtt.stateTopic(bd.FriendlyName)] = id
			nc.friendlyNameToDeviceID[bd.FriendlyName] = id
			nc.mu.Unlock()
		}
		return
	}

	builder, ok := nc.classify(bd)
	if !ok {
		nc.logger.Debug("skipping device with no supported exposes",
			zap.String("ieee_address", bd.IEEEAddress), zap.String("friendly_name", bd.FriendlyName))
		return
	}

	d, err := builder.build(bd)
	if err != nil {
		nc.logger.Error("unable to build device", zap.String("ieee_address", bd.IEEEAddress), zap.Error(err))
		return
	}

	override := nc.cfg.overrideFor(bd.IEEEAddress)
	id = override.ID
	if id == "" {
		id = "zigbee-" + strings.TrimPrefix(bd.IEEEAddress, "0x")
	}
	d.Id = id
	if d.Address == nil {
		d.Address = &device.Device_Address{}
	}
	d.Address.IsReachable = true // corrected by a subsequent availability-topic message, if any

	nc.mu.Lock()
	// A device id already claimed by a *different* physical device (almost always a colliding id
	// override in config) is refused rather than silently overwritten - see
	// bridges/zwave/network.go's identical check for the full rationale. This can only be the
	// "different physical device" case by construction: bd.IEEEAddress is confirmed not already
	// in nc.devices by the early-return above, so a hit here always belongs to some other device.
	existing, claimed := nc.devices[id]
	if claimed {
		nc.mu.Unlock()
		nc.logger.Error("device id already claimed by a different physical device; skipping - check for a colliding id override in config",
			zap.String("device_id", id), zap.String("ieee_address", bd.IEEEAddress), zap.String("existing_ieee_address", existing.ieeeAddress))
		return
	}

	newBD := &builtDevice{device: d, builder: builder, friendlyName: bd.FriendlyName, ieeeAddress: bd.IEEEAddress}
	nc.devices[id] = newBD
	nc.ieeeToDeviceID[bd.IEEEAddress] = id
	nc.friendlyNameToDeviceID[bd.FriendlyName] = id
	topic := nc.mqtt.stateTopic(bd.FriendlyName)
	if ownerID, claimed := nc.topicToDeviceID[topic]; claimed && ownerID != id {
		nc.logger.Warn("state topic claimed by more than one device; updates will only route to the most recently matched device",
			zap.String("friendly_name", bd.FriendlyName), zap.String("previous_device_id", ownerID), zap.String("device_id", id))
	}
	nc.topicToDeviceID[topic] = id
	nc.mu.Unlock()

	nc.zb.registerDevice(id, nc)
	d.Version = computeVersion(d)
	nc.svc.UpdateDevice(d)
}

// classify maps a zigbee2mqtt device's exposes list to the deviceBuilder that owns it. Unlike
// bridges/zwave's classify (a fixed generic-device-class switch), Zigbee has no equivalent
// taxonomy to key off - so this inspects the actual exposes shape instead: a top-level "light"
// composite is a controllable light; a top-level "fan" composite (with at least a "state"
// feature) is a fan; a top-level "switch" composite, or a bare top-level binary "state" property,
// is a controllable on/off device; anything else is only a candidate sensor, built if it reports
// at least one of the read-only capabilities sensorBuilder knows about. The Coordinator entry, a
// disabled device, and a device with no exposes at all (never interviewed, or a model
// zigbee-herdsman-converters doesn't recognize) never match anything here.
func (nc *networkConn) classify(bd bridgeDevice) (deviceBuilder, bool) {
	if bd.Type == "Coordinator" || bd.Disabled || bd.Definition == nil || len(bd.Definition.Exposes) == 0 {
		return nil, false
	}
	exposes := bd.Definition.Exposes

	if lb, ok := newLightBuilder(exposes); ok {
		return lb, true
	}
	if fb, ok := newFanBuilder(exposes); ok {
		return fb, true
	}
	if _, ok := findTopLevel(exposes, "switch"); ok {
		return onOffBuilder{}, true
	}
	if _, ok := findByProperty(exposes, "state"); ok {
		return onOffBuilder{}, true
	}

	for _, prop := range sensorProperties {
		if _, ok := findByProperty(exposes, prop); ok {
			return sensorBuilder{}, true
		}
	}
	return nil, false
}

// handleAvailability applies a <friendly_name>/availability update to the owning device's
// Address.IsReachable.
func (nc *networkConn) handleAvailability(friendlyName string, payload []byte) {
	online, ok := decodeAvailability(payload)
	if !ok {
		return
	}

	nc.mu.Lock()
	deviceID, ok := nc.friendlyNameToDeviceID[friendlyName]
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
	bd.device.Address.IsReachable = online
	nc.svc.UpdateDevice(bd.device)
}

// applyCommand routes a command to the device it targets and, on success, returns the
// optimistically-updated device - same contract as bridges/zwave/network.go's applyCommand. The
// returned device is a clone taken while still holding bd's lock, so the caller can't race with a
// concurrent state update mutating the same *device.Device.
func (nc *networkConn) applyCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	nc.mu.Lock()
	bd, ok := nc.devices[cmd.DeviceId]
	nc.mu.Unlock()
	if !ok {
		return nil, bridge.ErrDeviceNotFound
	}

	bd.mu.Lock()
	defer bd.mu.Unlock()

	if err := bd.builder.applyCommand(ctx, nc.mqtt, bd.friendlyName, bd.device, cmd); err != nil {
		return nil, err
	}
	bd.device.Version = computeVersion(bd.device)
	return proto.Clone(bd.device).(*device.Device), nil
}
