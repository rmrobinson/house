package main

import (
	"fmt"
	"hash/fnv"

	"github.com/rmrobinson/house/api/device"
)

// computeVersion hashes the subset of d's fields that are targets of a Command this bridge
// accepts, plus Device.Config, per Device.version's documented contract
// (api/device/device.proto) - mirrors bridges/cast/version.go and bridges/webos/version.go's
// ComputeVersion. Called by network.go every time a device is (re)built or its state changes, so
// service/bridge/api.go's checkVersion has something real to compare a Command.version against
// instead of every zwave device reporting an empty version forever.
//
// Included: config.name, config.description (no zwave command sets these today, but every
// device carries a Config per the shared contract, and SetBridgeConfig-style per-device naming
// may land later); generic.on_off.state.is_on (switchBuilder); light.on_off.state.is_on,
// light.brightness.state.level, light.colour.state.rgb (dimmerBuilder/rgbLightBuilder).
//
// Excluded: everything under Sensor - sensorBuilder.applyCommand always returns
// bridge.ErrUnsupportedCommand, so a Sensor device has no command-target fields at all; and, per
// the contract, address.* (including is_reachable) and any other pure telemetry.
//
// This is a change detector, not a security boundary - FNV-64a is sufficient.
func computeVersion(d *device.Device) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "name=%s\x00description=%s\x00", d.GetConfig().GetName(), d.GetConfig().GetDescription())

	if g := d.GetGeneric(); g != nil {
		fmt.Fprintf(h, "on_off=%t\x00", g.GetOnOff().GetState().GetIsOn())
	}
	if l := d.GetLight(); l != nil {
		rgb := l.GetColour().GetState().GetRgb()
		fmt.Fprintf(h, "on_off=%t\x00brightness=%d\x00rgb=%d,%d,%d\x00",
			l.GetOnOff().GetState().GetIsOn(),
			l.GetBrightness().GetState().GetLevel(),
			rgb.GetRed(), rgb.GetGreen(), rgb.GetBlue(),
		)
	}

	return fmt.Sprintf("%x", h.Sum64())
}
