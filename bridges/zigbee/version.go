package main

import (
	"fmt"
	"hash/fnv"
	"sort"

	"github.com/rmrobinson/house/api/device"
)

// computeVersion hashes the subset of d's fields that are targets of a Command this bridge
// accepts, plus Device.Config, per Device.version's documented contract
// (api/device/device.proto) - mirrors bridges/zwave/version.go and bridges/nanoleaf/version.go.
// Called by network.go every time a device is (re)built or its state changes.
//
// Included: config.name, config.description; generic.on_off.state.is_on (onOffBuilder);
// light.on_off.state.is_on, light.brightness.state.level, light.colour.state.hsb,
// light.colour.state.colour_temperature_k (lightBuilder); fan.on_off.state.is_on,
// fan.mode.state.current_mode, fan.toggles.state.settings (fanBuilder) - the Fan branch was
// missing entirely for a while after fanBuilder was added, which would have left every Fan
// device's version constant regardless of applied commands, defeating Command.version's whole
// purpose for that device type; caught by TestComputeVersion_FanIncludesOnOffModeAndToggles.
//
// Excluded: everything under Sensor - sensorBuilder.applyCommand always returns
// bridge.ErrUnsupportedCommand, so a Sensor device has no command-target fields at all;
// generic.power (onOffBuilder's optional metering trait is read-only telemetry, not a command
// target); fan.speed (fanBuilder always builds this with Attributes.can_control left false - it's
// read-only telemetry from a separate diagnostic property, not something any Command can move -
// see fanBuilder's doc comment); and, per the contract, address.* (including is_reachable) and
// any other pure telemetry.
//
// This is a change detector, not a security boundary - FNV-64a is sufficient.
func computeVersion(d *device.Device) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "name=%s\x00description=%s\x00", d.GetConfig().GetName(), d.GetConfig().GetDescription())

	if g := d.GetGeneric(); g != nil {
		fmt.Fprintf(h, "on_off=%t\x00", g.GetOnOff().GetState().GetIsOn())
	}
	if l := d.GetLight(); l != nil {
		hsb := l.GetColour().GetState().GetHsb()
		fmt.Fprintf(h, "on_off=%t\x00brightness=%d\x00hsb=%d,%d\x00ct=%d\x00",
			l.GetOnOff().GetState().GetIsOn(),
			l.GetBrightness().GetState().GetLevel(),
			hsb.GetHue(), hsb.GetSaturation(),
			l.GetColour().GetState().GetColourTemperatureK(),
		)
	}
	if f := d.GetFan(); f != nil {
		fmt.Fprintf(h, "on_off=%t\x00mode=%s\x00", f.GetOnOff().GetState().GetIsOn(), f.GetMode().GetState().GetCurrentMode())
		// map iteration order is randomized in Go - sort the toggle names first so the hash is
		// deterministic across calls for the same logical state, not just within one call.
		settings := f.GetToggles().GetState().GetSettings()
		names := make([]string, 0, len(settings))
		for name := range settings {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(h, "toggle.%s=%t\x00", name, settings[name])
		}
	}

	return fmt.Sprintf("%x", h.Sum64())
}
