# webOS bridge

Bridges LG webOS smart TVs into the `house` ecosystem over webOS's SSDP discovery + WebSocket +
`ssap://` command protocol (the same protocol the LG ThinQ/Smart TV mobile app uses).

## Device model

Each TV is exposed as a `Television` device (`api/device/television.proto`), the same device
type `bridges/roku` uses. `OnOff`/`Volume`/`App`/`Channel` are populated once pairing succeeds;
`Media` is left unset - webOS doesn't reliably report now-playing info for arbitrary apps, the
same deferral `bridges/roku` already made for its own `Media` trait.

Power is deliberately *not* modeled as a bespoke enum. The existing `OnOff.State.is_on` (screen on
vs. off) plus `Device.Address.is_reachable` (socket alive vs. not) together express every power
mode this TV protocol exposes - see `normalize.go`'s doc comment for the mapping table. A TV in
full standby (socket closed) is reported `is_reachable: false`, `CHANGED` rather than `REMOVED` -
the device isn't gone, it's asleep.

## Pairing

A newly discovered TV has no `client_key` yet. The first command or connection attempt triggers
webOS's on-screen accept prompt; once a human accepts it, the bridge persists the issued
`client_key` back to config (keyed by the TV's own `webos_uuid` - see `webos.example.yaml`) and
every subsequent connection skips the prompt - confirmed live against a real webOS 5.5 set, but
only after fixing a real bug where this wasn't happening (see "Verified live" below).

The permission manifest sent during pairing (`bridges/lib/webosctrl/payloads.go`'s
`manifestPermissions`) is scoped to the four required capabilities (volume, app launch/close,
channel) plus the minimum read-back needed to populate `Television` state. **Widening it later
forces every already-paired TV through a fresh pairing prompt** - LG freezes a client-key to
whatever permission set it was issued under - so this list isn't meant to grow casually.

## Wake-on-LAN

`SetOnOff(true)` against a TV in full standby (no live connection at all) requires a `mac:` entry
in that device's config - webOS TVs don't advertise their MAC over SSDP, so this is a one-time
manual lookup (see `webos.example.yaml`). Without it, powering on from full standby isn't possible;
`SetOnOff(true)` against a TV that's merely screen-off (active-standby, socket still alive) works
without a MAC either way.

## Verified live

Confirmed against a real LG webOS 5.5 TV: discovery, pairing, `Television` state read-back
(OnOff/Volume/App including the live foreground app), Volume (absolute + relative)/Mute, App
launch, and OnOff(false)/(true) including the Wake-on-LAN path. That session found and fixed three
real bugs, all now covered by regression tests:

- **`turnOffScreen` 404s on this firmware.** `ssap://com.webos.service.tvpower/power/turnOffScreen`
  returned `404 no such service or method` even though `getPowerState` on the same service works
  fine - so it's a genuinely missing method on this build, not a permission gap (that'd be a 401).
  `SetOnOff(false)` now tries `TurnOffScreen` first and falls back to a full `ssap://system/turnOff`
  standby on rejection (`bridges/webos/command.go`'s `turnOff`). The tradeoff: full standby closes
  the socket, so waking back up needs Wake-on-LAN (`mac:` in config) rather than the
  lower-latency active-standby `TurnOnScreen` path.
- **Silent power-loss wasn't detected at all.** The TV going into full standby via the fallback
  above closed its side without ever sending a WS close frame, so `readLoop`'s `ws.ReadMessage()`
  blocked forever - `Reachable` never flipped false, and the next `SetOnOff(true)` skipped
  Wake-on-LAN entirely (assumed it was still connected) and just timed out. Fixed with a WS
  ping/pong heartbeat in `bridges/lib/webosctrl/conn.go` (mirrors `bridges/lib/castctrl`'s
  heartbeat), declaring the connection dead after two missed pongs (~10s).
- **`client_key` was never actually being reused.** `deviceConfig`'s fields only had
  `mapstructure` tags; `viper.WriteConfig` marshals via `yaml.Marshal`, which ignores those tags
  and lowercases field names instead (writing `clientkey`/`hastuner`), which then didn't match the
  `mapstructure:"client_key"`/`"has_tuner"` tags `UnmarshalKey` looks for on the next read. Every
  restart silently discarded the persisted key and re-paired from scratch. Fixed by adding
  matching `yaml` tags to `deviceConfig` (`bridges/webos/bridge.go`).

## Known-unverified areas

The following are still best-effort implementations against public community-client precedent
(LG doesn't document this protocol), not confirmed behavior:

- Whether the permission manifest actually authorizes every targeted endpoint beyond what's listed
  above as verified.
- Channel/tuner endpoint behavior on a tuner-less (HDMI/ARC-only) setup - `has_tuner: false` in
  config is the escape hatch if `ssap://tv/...` calls misbehave.
- `AppClose`, `ChannelAbsolute`/`ChannelRelative` - added after the live session above, not yet
  exercised against real hardware.

## Config

See `webos.example.yaml`. Unlike `bridges/cast`'s opt-in-once mDNS, SSDP discovery here always
runs on `bridge.refresh_interval` - there's no way to pre-populate an unpaired device's
`client_key`, so discovery is core to operation rather than an optional convenience.
