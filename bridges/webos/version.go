package main

import (
	"fmt"
	"hash/fnv"

	"github.com/rmrobinson/house/api/device"
)

// ComputeVersion hashes the subset of d's fields that are targets of a
// Command this bridge accepts, per Device.version's documented contract
// (api/device/device.proto).
//
// Included: on_off.state.is_on (OnOff); volume.state.level, volume.state.
// is_muted (VolumeAbsolute/VolumeRelative/Mute); app.state.application_id
// (AppLaunch/AppClose); channel.state.current_channel.id (ChannelAbsolute/
// ChannelRelative).
//
// Excluded, specifically: last_seen, address.* (reachability telemetry, no
// command touches these - see the plan doc's state-mapping table, which
// deliberately encodes power nuance via is_reachable rather than a new
// field); app.state.application_name/application_status and
// channel.state.current_channel.number/name (display-only fields a command
// doesn't target, even though they move in lockstep with the ids above -
// including them would invalidate the version on every relabel without a
// corresponding command ever having run).
//
// This is a change detector, not a security boundary - FNV-64a is sufficient.
func ComputeVersion(d *device.Device) string {
	tv := d.GetTelevision()

	h := fnv.New64a()
	fmt.Fprintf(h, "is_on=%t\x00volume_level=%d\x00volume_muted=%t\x00app_id=%s\x00channel_id=%s\x00",
		tv.GetOnOff().GetState().GetIsOn(),
		tv.GetVolume().GetState().GetLevel(),
		tv.GetVolume().GetState().GetIsMuted(),
		tv.GetApp().GetState().GetApplicationId(),
		tv.GetChannel().GetState().GetCurrentChannel().GetId(),
	)

	return fmt.Sprintf("%x", h.Sum64())
}
