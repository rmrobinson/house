package main

import (
	"fmt"
	"hash/fnv"

	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
)

// ComputeVersion hashes the subset of d's fields that are targets of a
// Command this bridge accepts, plus Device.Config, per Device.version's
// documented contract (api/device/device.proto). Everything else —
// telemetry, and fields no command here controls — is excluded, so the
// version doesn't invalidate on every tick of normal playback.
//
// Included: config.name, config.description (UpdateDeviceConfig);
// media.state.playback_state (Playback); volume.state.level, volume.state.
// is_muted (VolumeAbsolute/VolumeRelative/Mute).
//
// Excluded, specifically: last_seen, address.* (reachability telemetry, no
// command touches these); media.state.playback_position_s and
// playback_position_updated_at (nominally a SeekAbsolute/SeekRelative
// target, excluded anyway — a playing device moves this continuously as a
// side effect of playing, not of being seeked, so including it would
// invalidate the version on every tick regardless of whether any command
// ran); media.state.media_type and all populated *_details,
// media.state.playback_length_s, app.state.* (this bridge has no command
// that changes what's playing or what app is running, so gating a write on
// these would make every command racy against activity house never
// initiated and can't prevent).
//
// This is a change detector, not a security boundary — FNV-64a is sufficient.
func ComputeVersion(d *device.Device) string {
	var media *trait.Media
	var volume *trait.Volume
	if mp := d.GetMediaPlayer(); mp != nil {
		media = mp.GetMedia()
		volume = mp.GetVolume()
	} else if tv := d.GetTelevision(); tv != nil {
		media = tv.GetMedia()
		volume = tv.GetVolume()
	}

	h := fnv.New64a()
	fmt.Fprintf(h, "name=%s\x00description=%s\x00playback_state=%d\x00volume_level=%d\x00volume_muted=%t\x00",
		d.GetConfig().GetName(),
		d.GetConfig().GetDescription(),
		media.GetState().GetPlaybackState(),
		volume.GetState().GetLevel(),
		volume.GetState().GetIsMuted(),
	)

	return fmt.Sprintf("%x", h.Sum64())
}
