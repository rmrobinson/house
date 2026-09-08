# cast bridge

Bridges Google Cast (CASTV2) devices — Chromecasts, Google Home/Nest speakers, Google TV
Streamer — into the `house` ecosystem, exposing live now-playing state and transport/volume
control. Entirely local: no Google account, no cloud, no OAuth. The wire protocol itself
(`bridges/lib/castctrl`) is a standalone library with no `house`/proto dependency; this package
is the adapter that turns it into a `bridge.Handler`.

## Config: static, optional mDNS discovery, or both

Every device can be listed by IP and Cast UUID in config (`cast.example.yaml`) — `kind` (media
player vs. television) is then whatever the config says. Set `cast.discovery.mdns_enabled: true`
to also (or instead) have the bridge do a single mDNS browse for `_googlecast._tcp` at startup,
using `devices.go`'s `DeviceKind()` to infer `kind` from the `ca` TXT record's bit 0 and the `fn`
TXT record for a friendly name (`discover.go`). Config-listed devices stay authoritative —
discovery only appends UUIDs not already listed and refreshes the host address of ones that are;
see `mergeDiscovered`'s doc comment. It's a one-shot browse at process start, not a background
poller, so a device that comes online after the bridge has already started needs a restart (or a
config entry) to be picked up.

**mDNS is unauthenticated.** A malicious device on the same LAN segment can advertise a
`_googlecast._tcp` record claiming any UUID it likes, including one belonging to a real,
already-configured device — `mergeDiscovered` would refresh that device's host to whatever address
the spoofed record points at, and the bridge would connect there instead. This isn't detectable at
the TLS layer either, since Cast connections already use `InsecureSkipVerify` by design (self-signed,
frequently-regenerated device certs — see `conn.go`). Enabling `cast.discovery.mdns_enabled` widens
this from "an attacker needs to redirect traffic to a device's already-known IP" (e.g. ARP spoofing)
to "an attacker just needs to send one mDNS packet." Leave discovery off on any network where that
distinction matters, and rely on `cast.devices` with pinned IPs instead.

mDNS (`_googlecast._tcp.local`) doesn't cross VLANs without a reflector — if this bridge runs on a
protected VLAN separate from client devices and there's no reflector, discovery won't see anything
and `cast.devices` is the only way in. Cast speaker groups (`md=Google Cast Group` in mDNS) aren't
filtered out of discovery results — group pseudo-devices will show up like any other unless
`cast.devices` deliberately omits them (discovery only adds, never removes) or the mDNS TXT record
happens not to carry an `id` field, in which case `discoverDevices` already skips it.

### Finding a device's UUID without discovery enabled

The Cast UUID isn't printed anywhere in the Google Home app. Easiest ways to get it without
enabling `cast.discovery.mdns_enabled`:

- Your router's DHCP client list often shows the mDNS hostname, e.g. `Kitchen-Speaker-abcd1234`
  — decoding it isn't reliable, but it confirms which IP is which device.
- `avahi-browse -r _googlecast._tcp` (Linux, with `avahi-utils` installed) or `dns-sd -Z
  _googlecast._tcp local.` (macOS) on a host that *is* on the same L2 segment as the device —
  one-time use, doesn't need to run continuously — prints the `id=` TXT record, which is the UUID
  this bridge wants (no dashes).
- If you've ever used `pychromecast` or `go-chromecast` against the device from a machine on the
  same segment, either tool's discovery output includes the UUID too.

## Supported commands

| Command | Requires |
|---|---|
| `Playback` (PLAY/PAUSE/STOP) | An active media session (`AppConnected`, media playing/paused) |
| `SeekAbsolute` / `SeekRelative` | Same as above |
| `VolumeAbsolute` / `VolumeRelative` | Receiver connected (works even with no app running) |
| `Mute` | Same as above |
| `AppLaunch` | Receiver connected (no app needs to already be running) |

The table above applies equally to `Television`-kind devices (`Playback`/`SeekAbsolute`/
`SeekRelative`/`VolumeAbsolute`/`VolumeRelative`/`Mute`/`AppLaunch`; `OnOff` isn't wired up for
either kind — `Normalize()` never sets `Television.OnOff`, so it's always rejected as unsupported).

Confirmed live against a real Google TV Streamer: `VolumeRelative` and `Mute` both round-trip
successfully at the protocol level (the receiver acknowledges the new level/muted state, and
`bridgecli`/`ListDevices` reflect it) — but this hardware reports `Volume.controlType: "fixed"`
(`can_control` unset on the `Volume` trait, same as the confirmed-`fixed` unit under "Confirmed
live against the full tested fleet" below), and neither command produces any actual audio change
on the device: it instead shows an on-screen prompt to use the physical remote. This isn't a bug
in this bridge — the TV/soundbar owns volume via HDMI-CEC, not the Cast receiver, on this
controlType — but it does mean `VolumeAbsolute`/`VolumeRelative`/`Mute` are effectively no-ops on
any Cast-driven TV reporting `fixed`, regardless of what the protocol response claims. Check
`can_control` on the `Volume` trait before surfacing volume controls for a `Television`-kind
device in a client.

`VolumeAbsolute` on a device reporting `controlType: "master"` is issued as a sequence of
single-step `SET_VOLUME` calls computed from the last-known level, since `master` devices don't
honor an arbitrary absolute level reliably. This is fragile if the volume changes concurrently
from another sender (phone, voice assistant) between read and write — see the comment on
`setVolumeAbsolute` in `command.go`. Confirmed live against the full tested fleet (identified
correctly via mDNS `id`/`fn`, not guessed from which IPs happened to answer a manual port scan —
an earlier pass at this got Chromecast Audio and Google TV Streamer backwards): Google Home Mini
and Chromecast Audio both report `controlType: "master"`; Google TV Streamer reports `controlType:
"fixed"` (`can_control` correctly `false` — the TV/soundbar owns volume, not the Cast receiver).

`SeekAbsolute` past the current track's length isn't rejected or clamped by the device — confirmed
live against a real Chromecast/Spotify session, it silently skips to a different track in the
queue instead. See the doc comments on `castctrl.Session.SeekAbsolute` and the `SeekAbsolute` case
in `dispatchCommand` (`command.go`).

`AppLaunch` starts an application by its Cast receiver app ID (e.g. `233637DE` for YouTube) — there's
no local API to enumerate what a device *could* run, only what it's running now (`App.Attributes.
applications` stays empty), so the caller has to already know the ID. Confirmed live against a real
Google TV Streamer for two IDs: `233637DE` (YouTube) and `9AC194DC` (Plex) — both come back
correctly reflected in `App.State` (`application_id`/`application_name`). **`AppLaunch` alone doesn't
start playback** — confirmed live, both IDs above land on the same "Ready to Cast" idle screen once
launched, not a browsable/interactive app UI. Starting real content needs a `LOAD` message (content
URL/ID, MIME type, stream type — for an authenticated source like Plex, probably also a
token/header) that this bridge never sends: `LOAD` isn't implemented anywhere in this bridge or
`castctrl` — `TypeLoad` exists in `namespaces.go` as a constant but nothing ever sends it. Whether
any Cast receiver app has a browsable on-TV UI reachable via `LAUNCH` alone (without `LOAD`) is
untested here — the two apps tried both needed `LOAD` regardless. Stopping the running application
isn't wired up either
(`Playback`'s `ACTION_STOP` ends the *media* session within an app, not the app itself). Per-command
capability granularity beyond the four `can_pause`/`can_seek`/`can_skip_forward`/`can_skip_backward`
flags (queue, shuffle, repeat, like — real capabilities several apps report but nothing in
`trait.Media` models yet) is out of scope.

## Album art proxy

Optional (`cast.art_base_url` / `cast.art_listen_port` in config) — see `art.go`. If configured,
every art URL a device reports is rewritten to a locally-served, cached copy before the device
update is published, so dashboard clients never need direct internet egress. Deliberately kept
free of any Cast-specific dependency (`art.go` only knows about URLs and bytes) — cameras will
likely want the same facility later, at which point it should move to a shared `house` package
rather than staying bridge-local.

## Config

See `cast.example.yaml`. Config is loaded once at startup; there's no hot-reload.

## Running

```sh
bazel build //bridges/cast
bazel-bin/bridges/cast/cast_/cast   # run from the directory containing your config
```

Then, using `bridgecli` (`bazel build //clients/bridgecli`):

```sh
bridgecli --addr 127.0.0.1:17011 bridge --bridgeID <bridge ID> listDevices
bridgecli --addr 127.0.0.1:17011 device --deviceID <cast uuid> volume --delta -1
bridgecli --addr 127.0.0.1:17011 device --deviceID <cast uuid> mute --muted=true
bridgecli --addr 127.0.0.1:17011 device --deviceID <cast uuid> app launch --appID 233637DE
```

Confirm the command actually reached the device (not just that the bridge's own optimistic state
updated) by watching the device's own display/speaker, or the Google Home app.

### Troubleshooting

- `connection refused` on the Cast device's port 8009 — wrong IP, or the device is on a different
  VLAN this bridge can't route to.
- Session stuck at `receiver_connected`, never reaching `app_connected` despite something actively
  casting — check the bridge's logs for `receiver get_status failed`; a device that's dropped its
  TLS session mid-handshake looks like this until the next heartbeat-driven reconnect.
- Volume commands silently no-op or move by only one step no matter the requested level — expected
  for `controlType: "fixed"` devices (typically a Chromecast whose audio output is owned by an
  attached TV/receiver) — `can_control` on the `Volume` trait will read `false` for these, and no
  amount of step-synthesis fixes that; there's no volume path to synthesize from.
