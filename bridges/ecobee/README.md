# ecobee bridge

Acts as a HomeKit *controller* (the role Apple Home normally plays) and pairs directly with a
single ecobee thermostat over its local HomeKit Accessory Protocol (HAP) API, exposing it and its
remote sensors as `house` devices. State is refreshed via polling; this bridge does not run its
own HomeKit accessory server.

This bridge was built and tested against one specific unit - a Smart Thermostat Premium, firmware
4.10.330030 - not a generic multi-model ecobee integration. See "If you're pairing a different
ecobee" below before assuming any of the specifics here carry over.

## Why hand-rolled HAP, not a library

`bridges/lib/homekitctrl` implements HAP pairing/pair-verify/characteristic read-write itself
rather than depending on `github.com/mctofu/homekit` (the only maintained Go HAP controller
client found). That library's pair-verify request sends an extra TLV tag the HAP spec only
defines for pair-*setup*, not pair-*verify* - confirmed by direct testing, this specific ecobee
flatly rejects the whole request (HTTP 400, empty body) because of it, and the bug lives inside
unexported dialer internals that can't be patched from outside the library. `cmd/pair` still uses
that library for the one-time, human-supervised SRP pair-*setup* handshake, which has no known
bugs - see its own doc comment.

## Known gaps (confirmed, not just unverified)

- **No ERV/HRV control.** This ecobee doesn't expose ventilation on/off or runtime over HAP at
  all - `trait.Ventilation` exists in the API but this bridge only ever populates
  `filter_status_message` (see below), never `is_on`. If you need this, it requires ecobee's
  cloud API, not HomeKit.
- **No Vacation mode.** Only the three built-in comfort profiles (Home/Sleep/Away) are reachable
  over HAP. Compose an equivalent from `SetComfortProfile`/`SetHeatCoolSetpoints` plus your own
  scheduled task if you need this - see the original design notes for the reasoning.
- **No furnace/HVAC filter reminder**, despite the ventilator filter reminder being available
  (see below) - the full accessory characteristic dump has no equivalent characteristic for it
  anywhere on this unit. If a different ecobee model/firmware exposes one, it isn't wired up here.
- **No air quality (CO2/VOC).** Premium has the sensor hardware; it isn't exposed over HAP.
- **No humidity control.** Unlike the gaps above, `TargetRelativeHumidity` *is* writable over HAP
  on this unit - this is a deliberate choice, not a HAP limitation: this house has no humidifier
  attached, so a write here would be accepted by the accessory but do nothing. `command.SetHumidity`
  exists in the API but this bridge rejects it (`ErrUnsupportedCommand`); `target_humidity_percent`
  is still reported read-only in `State`. If a humidifier is ever added, wiring this back up is
  just restoring the `Command_SetHumidity` case in `command.go`'s switch (removed, not stubbed
  out - see git history) plus the bounds back onto `Attributes`.
- Writes (`SetTemperature`, `SetComfortProfile`, etc.) create a **temporary hold**, not a
  permanent schedule edit - the thermostat's own schedule resumes at its next transition (or per
  its own hold-duration preference, which isn't reachable over HAP). A setpoint reverting on its
  own later is expected behavior, not a bug.

## What is exposed

- `thermostat`: mode (off/heat/cool/auto), single or dual setpoints depending on mode, current
  temperature/humidity, target humidity (read-only - see "No humidity control" below), display
  units, active comfort profile, hold status, fan mode, and the thermostat's own built-in
  occupancy/motion sensing (Premium's radar "SmartSensor") - folded directly into the thermostat
  device rather than a separate one, since it's the same physical unit and the same HAP `aid`,
  unlike a remote sensor.
- Each paired remote sensor: temperature, battery, occupancy, motion - as its own `device.Sensor`.
- `trait.Ventilation.state.filter_status_message`: the ventilator's read-only filter-change
  reminder text, if this unit has one configured. Informational only (`can_control: false`).

## Setup

### 1. Pair

```sh
bazel build //bridges/ecobee/cmd/pair
bazel-bin/bridges/ecobee/cmd/pair/pair_/pair discover
```

Enable HomeKit on the thermostat itself first (Settings → HomeKit) if you haven't already - it
won't advertise over mDNS at all until you do. Note the ID `discover` reports and the setup code
shown on the thermostat's screen (**including the dashes** - `XXX-XX-XXX` is the literal password
HAP's pairing handshake uses, not just a display format; a code with the dashes stripped will
fail pairing with an "Authentication Failed" error that has nothing to do with a typo).

```sh
bazel-bin/bridges/ecobee/cmd/pair/pair_/pair pair --id <id-from-discover> --pin <XXX-XX-XXX>
bazel-bin/bridges/ecobee/cmd/pair/pair_/pair list   # confirm it saved
```

This writes `ecobee-pairing.json` (path controlled by `--store`, same path the bridge's own
`ecobee.pairing_store` config points at) in the current directory. This file contains this
controller's private key - keep it out of version control.

Pairing is a separate one-time binary rather than something exposed over the bridge's own gRPC
surface because it needs a PIN read off the physical accessory - there's no way to do that
through a remote API call. See the TODO on `cmd/pair pair` for the idea of eventually making this
a bridge-specific admin RPC instead.

### 2. Configure the bridge

Copy `ecobee.example.yaml`, fill in the accessory alias and any remote sensors (their `aid`s come
from `docs/ecobee-hap-dump.txt` - or capture your own, see below).

### 3. Run

```sh
bazel build //bridges/ecobee
bazel-bin/bridges/ecobee/ecobee_/ecobee   # run from the directory containing your config
```

```sh
bridgecli --addr 127.0.0.1:17011 bridge --bridgeID <bridge ID> listDevices
bridgecli --addr 127.0.0.1:17011 device --deviceID ecobee-main thermostatMode --mode cool
bridgecli --addr 127.0.0.1:17011 device --deviceID ecobee-main temperature --celsius 21.5
bridgecli --addr 127.0.0.1:17011 device --deviceID ecobee-main heatCoolSetpoints --heatCelsius 19 --coolCelsius 24
bridgecli --addr 127.0.0.1:17011 device --deviceID ecobee-main comfortProfile --profile Away
```

## If you're pairing a different ecobee

Every numeric characteristic ID (`iid`) in `mapping.go`/`sensor.go`, and the *meaning* of every
vendor (non-Apple-spec) one, is specific to this exact unit and firmware version - HAP doesn't
guarantee stable `iid` numbering across units, and vendor characteristics use undocumented UUIDs
that could differ entirely on another model. Don't assume this table carries over:

1. Pair against the new unit (above), then dump its full characteristic list. There's no
   `listCharacteristics`-equivalent built into this bridge (config-driven `iid`s were chosen over
   a runtime `/accessories` discovery layer, since this targets one specific house's one specific
   unit) - use `mctofu/homekit`'s own CLI for this one-off capture instead, patched the same way
   `cmd/pair` avoids its pair-verify bug (see that command's doc comment) - or ask whoever
   maintains this bridge for the exact repro steps used originally.
2. **Confirm vendor characteristic meanings by testing, not by reading values.** The comfort
   profile index (`charActiveComfortProfile`) on this unit is `0=Home, 1=Sleep, 2=Away`, and the
   heat/cool setpoint pairs are stored in a *different* order (Home, Away, Sleep) - confirmed by
   physically switching profiles on the thermostat and diffing before/after captures, not by
   inspecting static values. Assuming the two orderings match would silently write the wrong
   profile's setpoints. Do the same active-probing exercise on a new unit before trusting any
   vendor `iid`'s meaning.
3. Update the constants in `mapping.go`/`sensor.go` and re-run `bridges/ecobee`'s tests.

## Write reliability

`ProcessCommand` applies a command's effect optimistically and returns immediately - it does not
block on a synchronous re-read to verify the accessory actually applied it. Real-world reports
(and this bridge's own testing) show ecobee's local HAP writes can take noticeably longer to be
reflected back specifically when the originating write came from a HomeKit-based client, versus a
change made directly on the thermostat or through ecobee's own app. `Refresh`'s regular poll cycle
(`bridge.refresh_interval`, default 30s) is what actually reconciles this - if a command's effect
doesn't stick, expect it to visibly revert within one or two poll cycles rather than staying
applied forever, the same way ESPHome's bridge handles optimistic command application.
