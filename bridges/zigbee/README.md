# zigbee bridge

Bridges a Zigbee network into the `house` ecosystem via
[zigbee2mqtt](https://www.zigbee2mqtt.io/), talking to its MQTT gateway rather than a Zigbee radio
directly - the same shape of integration `bridges/zwave` uses for zwave-js-ui.

**Verified against a real zigbee2mqtt broker and a live, 24-device home Zigbee network** - see
"Verified against a live broker" below for what was checked and what wasn't. Everything not called
out there, or flagged as an assumption in "Known limitations", is still resting on zigbee2mqtt's
own published documentation rather than direct confirmation.

## Architecture

Like `bridges/zwave`, this bridge owns exactly one MQTT connection shared by the entire Zigbee
network - zigbee2mqtt already multiplexes every device's state onto that one broker connection.
Structurally, though, it differs from zwave-js-ui in two ways that simplify this bridge
considerably:

- **One topic per device, not one topic per property.** zigbee2mqtt publishes a device's *entire*
  known state as a single retained JSON object on `<base_topic>/<friendly_name>` (e.g.
  `{"state":"ON","brightness":150,"linkquality":60}`), and accepts a partial JSON object on
  `<base_topic>/<friendly_name>/set` to command it. There is no zwave-js-ui-style per-value topic
  scheme to build, and no `role -> topic` table to maintain per device - `network.go`'s
  `topicToDeviceID` only ever needs one entry per device. See `mqtt.go`'s `WriteState`.
- **Discovery is pushed, not pulled.** zwave-js-ui requires an explicit `getNodes` RPC call over
  MQTT; zigbee2mqtt instead publishes a retained `<base_topic>/bridge/devices` JSON array
  describing every paired device's model and capabilities (its "exposes"). Because it's retained,
  a fresh subscribe (which happens on every reconnect, since paho's session isn't persistent)
  redelivers it automatically - confirmed against a real broker - so this bridge doesn't need its
  own request/response/timeout machinery the way `bridges/zwave`'s `getNodes` does. A useful side
  effect: zigbee2mqtt also republishes this same retained message live, any time a device is
  paired, removed, or renamed - so unlike zwave-js-ui's connect-only discovery, this bridge picks
  up network topology changes during normal operation too, not only after a reconnect. See
  `network.go`'s `handleBridgeDevices`.

  **Unlike `bridge/devices`, an individual device's own `<base_topic>/<friendly_name>` state topic
  is generally *not* retained** - confirmed against a real broker running zigbee2mqtt's default
  `mqtt.retain: false`. Only `bridge/devices` (and a handful of other `bridge/*` metadata topics)
  are retained by zigbee2mqtt's own convention; per-device state is not, unless a deployment
  explicitly opts into `mqtt.retain: true`. Practically, this means a device's state after a
  reconnect is *not* recovered "for free" the way `bridge/devices` is - it stays at whatever this
  bridge last knew (or its trait's zero value, for a device discovered since the last update) until
  the device's own next report. See the next paragraph.

**Device classification is capability-based, not class-based.** Z-Wave has a fixed generic-device-
class taxonomy `bridges/zwave/network.go`'s `classify` can switch on; Zigbee has no equivalent.
Instead, `network.go`'s `classify` inspects a device's own `exposes` list (zigbee-herdsman-
converters' resolved capability description for that exact model) directly: a top-level `"light"`
composite expose is a controllable light, a top-level `"switch"` composite (or a bare top-level
`"state"` property) is a controllable on/off device, and anything else is only a candidate Sensor,
built if it reports at least one read-only capability this bridge recognizes. A practical
consequence: this bridge needs far less config-override machinery than `bridges/zwave` - Z-Wave's
Color Switch CC propertyKey numbering isn't standardized across products, but zigbee2mqtt's
exposes API already resolves that kind of ambiguity for every device model, so `config.go`'s
`deviceOverride` only carries a cosmetic/identity `id` override, never something required to make a
device buildable at all (contrast `bridges/zwave/config.go`'s required `colour_channels`).

**A freshly discovered device starts at its trait's zero value, and can stay there a while.**
`bridge/devices` describes a device's *capabilities*, not its current property values (unlike
zwave-js-ui's `getNodes`, which returns both together) - so `build` can't populate initial state
the way `bridges/zwave/devices.go`'s builders do. `applyState` corrects this once the device's own
state arrives - but, per the retention note above, that's whenever the device (or zigbee2mqtt's
periodic cluster reporting) next actually reports something, not immediately on subscribe. Confirmed
against a real broker: several battery-powered end devices (a door/window sensor, in this bridge's
test network) hadn't reported anything since zigbee2mqtt's own last restart, and sat at zero-value
traits for the bridge's entire test run; several mains-powered devices (dimmers, sensors with
scheduled reporting) did report within seconds, populating real state. Expect this gap to be
longest for infrequently-reporting battery devices, not a fixed "brief window."

## Supported device types

- **On/off devices** (switches, relays, smart plugs) → `Generic`/`OnOff`, from a top-level
  `"switch"` composite expose or a bare top-level `"state"` property. There's no dedicated
  `Switch`/`Outlet` device type in this repo yet, so this mirrors `bridges/zwave`'s `switchBuilder`
  choice. A device that also exposes `"power"` (a metering smart plug) gets `Generic.Power`
  populated too, reading `power`/`current`/`voltage` directly - still `Generic`, not a separate
  `Sensor`, since a metering plug is fundamentally one controllable device.
- **Lights** → `Light`/`OnOff` (+`Brightness` if the light's `"light"` expose reports a
  `"brightness"` feature, +`Colour` if it reports `"color_temp"` and/or `"color_hs"`), from a
  top-level `"light"` composite expose. Brightness is scaled from zigbee2mqtt's native 0-254 range
  (the expose's own `value_max`, defaulting to 254 if absent) to house's 0-100 percent.
  `"color_temp"` (mireds) converts to Kelvin via the fixed `mired = 1,000,000 / kelvin` relation -
  a physical conversion, not a device-specific guess. `"color_hs"` maps directly onto
  `trait.Colour_State_HSB` (hue 0-360, saturation 0-100) with no unit conversion needed. **Only
  `color_hs` is modeled** - a light that exposes `"color_xy"` (CIE 1931 chromaticity) instead gets
  no `Colour` trait at all; see "Known limitations" for why this isn't a hard build failure the way
  `bridges/zwave/devices.go`'s `rgbLightBuilder` refuses to build an unconfigured RGB bulb.
- **Sensors** → `Sensor`, populated with whichever of `Presence.occupancy_detected`
  (`"occupancy"`), `opened_closed` (`"contact"`, inverted - see below), `AirProperties`
  (`"temperature"`/`"humidity"`), `LightLevel` (`"illuminance_lux"`, preferred, or `"illuminance"`
  as a fallback - see below), `Battery` (`"battery"`, +`Metadata.LowBattery` from `"battery_low"`
  when present), `water` (`"water_leak"`), or `fire` (`"smoke"`) the device's top-level exposes
  actually report - same "only add a trait if the device actually reports it" pattern as
  `bridges/zwave/devices.go`'s `sensorBuilder`. `Sensor.Metadata.OnBattery` is set directly from
  `bridge/devices`' own `power_source` field (`"Battery"`) rather than inferred, which
  `bridges/zwave` has no equivalent field to do. Every trait here is read-only in zigbee2mqtt's own
  exposes access model; `applyCommand` always rejects, same as `bridges/zwave`'s `Sensor`.

  **`illuminance_lux` is preferred over `illuminance`** - confirmed against a real Philips Hue
  motion sensor (9290012607) whose exposes explicitly document `"illuminance"` as "Raw measured
  illuminance" (an uncalibrated sensor count, not lux) with `"illuminance_lux"` alongside it as the
  real calibrated reading. An earlier version of this bridge read `"illuminance"` directly as lux,
  which would have reported the wrong scale entirely for this device.
- **Fans** (including air purifiers) → `Fan`/`OnOff` (+`Speed`, +`Mode`, +`Toggles` as available),
  from a top-level `"fan"` composite expose with at least a `"state"` feature. Unlike
  onOffBuilder/lightBuilder's hardcoded `"state"` property name, a fan's actual wire property names
  are resolved per-device at classify-time (`newFanBuilder`) rather than assumed, since they're
  *not* a fixed convention - confirmed against a real IKEA STARKVIND air purifier (E2007), whose
  fan composite exposes on/off as `"fan_state"` and mode as `"fan_mode"`, not `"state"`/`"mode"`.
  `Speed` is modeled as read-only telemetry from a separate top-level `"fan_speed"` sibling
  property (confirmed not independently settable on the STARKVIND - its only speed control is the
  `Mode` enum below), not a second way to control the fan. `Mode` is populated from the fan
  composite's `"mode"` enum feature when present (`"off"`/`"auto"`/`"1"`..`"9"` on the STARKVIND).
  `Toggles` is populated only from `fanToggleCandidates` (`"led_enable"`, `"child_lock"` today) -
  a deliberately narrow, confirmed-on-real-hardware list, not a generic "every settable binary
  expose" heuristic - see "Known limitations". Each toggle's (and the state feature's own) raw
  on/off wire value is read from the expose's own `value_on`/`value_off` rather than assumed to be
  a JSON bool: confirmed necessary against the STARKVIND itself, whose `"led_enable"` is a plain
  bool but `"child_lock"` is the string pair `"LOCK"`/`"UNLOCK"` - both on the very same device.

Out of scope for this pass (mirroring `bridges/zwave`'s "Out of scope for this pass"): covers,
locks, climate/thermostat devices, and multi-endpoint devices (e.g. a 2-gang wall switch) -
`network.go`'s `findTopLevel`/`findByProperty` only ever match an expose with no `endpoint` set, so
a multi-gang device's second+ endpoints are simply invisible to this bridge, not partially modeled.

## Behavior notes

- **No restore-on-level bookkeeping.** `bridges/zwave`'s dimmer/RGB builders track
  `lastNonZeroLevel` so a plain "on" command restores the last brightness, because the Multilevel
  Switch CC's on/off state has no independent memory of level. The Zigbee cluster library's On/Off
  and Level Control clusters are genuinely independent - a compliant bulb restores its own last
  level when told `{"state":"ON"}` with no brightness in the payload - so `lightBuilder`'s plain
  on/off command never touches brightness at all, and needs no equivalent of
  `resolveRestoreOnLevel`. This is a protocol-level fact, not a live-hardware-dependent guess.
- **`WriteState`'s echo-wait reads only the first message after a write**, same trade-off
  `bridges/zwave`'s `WriteValue` makes and for the same reason (no per-write correlation id
  exists): if a device reports something unrelated before its real echo, this surfaces as a false
  "did not accept" error rather than retrying. See `mqtt.go`'s `WriteState` doc comment.
- **`Device.version` is populated** (`version.go`'s `computeVersion`, mirroring
  `bridges/zwave`/`bridges/nanoleaf`'s `computeVersion`), hashing every field a command targets
  (`OnOff`/`Brightness`/`Colour` state for `Generic`/`Light`; `OnOff`/`Mode`/`Toggles` state for
  `Fan`; plus `Device.Config`). `Generic.Power`/`Fan.Speed` (read-only telemetry) and everything
  under `Sensor` (no command-target fields at all) are excluded, same contract as the other
  bridges. `computeVersion` had no `Fan` branch at all for a while after `fanBuilder` was added -
  caught by adversarial review (`TestComputeVersion_FanIncludesOnOffModeAndToggles`), not live
  testing, since a version that never changes doesn't surface as a visible failure the way a
  rejected command does - it would have left `Command.version`'s whole optimistic-concurrency
  check silently inert for every `Fan` device.
- **A light's brightness scale is read from its own `"brightness"` expose's `value_max`**
  (`newLightBuilder`, resolved at classify-time, same pattern `fanBuilder` uses for its
  per-device property names) - every light checked against a live broker happened to report the
  Zigbee-typical `254`, which masked an earlier version of this bridge hardcoding
  `zigbeeDefaultBrightnessMax` unconditionally in both directions (state decode and command
  encode) regardless of what a device's own expose actually reported, contradicting this same
  section's documented claim even then. Caught by adversarial review, not live testing, for the
  same reason as the `Fan` version gap above - see
  `TestLightBuilder_ApplyState_UsesDeviceValueMax`/`_ApplyCommand_Brightness_UsesDeviceValueMax`.
- **An already-known device is never rebuilt from a later `bridge/devices` message** - only a
  `friendly_name` rename is applied (see `buildDevice`'s doc comment). `bridge/devices` carries a
  device's capabilities only, never its current property values, and zigbee2mqtt republishes it
  live on *any* device pairing, removal, or rename anywhere on the network - an earlier version of
  this bridge rebuilt unconditionally on every such message, which meant pairing one new device
  reset every other already-known device's live state back to zero. The trade-off: a device's
  exposes are assumed stable for the life of the bridge process, so a mid-run firmware change to a
  device's capabilities won't be picked up without a restart. `buildDevice`'s rename path mutates
  `friendlyName` under the `*builtDevice`'s own lock, not `networkConn.mu` - an earlier version of
  the fix mutated it directly under `networkConn.mu` instead, a real data race against
  `applyCommand`'s read of the same field under the other lock (caught by
  `TestBuildDevice_RenameRaceWithApplyCommand` under `go test -race`, not by any wrong value - a
  race is a race regardless of which side loses it).
- **`service/bridge/api.go`'s `deviceSupportsCommand` had no branch for `Fan` at all** until this
  bridge's `fanBuilder` became the first `device.Fan` producer in this repo - every command against
  any `Fan` device, from any bridge, was rejected before ever reaching a bridge's `ProcessCommand`.
  Found via live testing against a real IKEA STARKVIND air purifier (a `bridgecli onoff` call
  failed with "the device does not support the specified command" despite `fanBuilder.applyCommand`
  itself being correct and unit-tested) and fixed in that shared file, not worked around here - the
  same kind of cross-cutting gap `bridges/nanoleaf` found and fixed for `AppLaunch`/`Light.Scene`.

## Config overrides

`zigbee.devices` is keyed by `ieee_address` (zigbee2mqtt's stable per-physical-device identifier -
unlike `friendly_name`, which a user can rename at any time). Every field is optional; today the
only one is `id`, which overrides the auto-assigned house device id
(`zigbee-<ieee_address, "0x" prefix stripped>`). See `zigbee.example.yaml`.

## Known limitations

- **Not everything below has been verified against real hardware** - see "Verified against a live
  broker" for exactly what was and wasn't. Anything flagged as an "assumption" here is a plausible
  first thing to check if this bridge's behaviour doesn't match a real deployment.
- **`color_xy`-only lights get no colour control at all**, rather than either converting xy to
  hue/saturation (lossy, and unverified without a real device to check the conversion against) or
  refusing to build the whole device the way `bridges/zwave/devices.go`'s `rgbLightBuilder` does
  for an unconfigured RGB bulb. A `color_xy`-only light is still a perfectly good dimmable light,
  so on/off/brightness control isn't sacrificed for a colour capability this bridge chooses not to
  guess at.
- **Multi-endpoint devices (multi-gang switches, etc.) are not modeled.** `classify` only inspects
  top-level exposes with no `endpoint` set - see "Supported device types" above.
- **A battery-powered remote/scene-controller with no other recognized capability still builds as a
  near-empty `Sensor`** - `sensorProperties` includes `"battery"` as a standalone match, and
  `classify` has no way to tell "a sensor that happens to run on a battery" apart from "a controller
  that happens to report a battery level." Confirmed live: a real Philips Hue dimmer switch
  (RWL020) on the network this bridge was checked against exposes `"battery"` alongside its actual
  capability, an `"action"` enum of button-press events (`on_press`, `up_hold_release`, etc.) - since
  no trait in this repo models button/scene actions at all, this device builds as a `Sensor` with
  only `Battery` populated, silently dropping 100% of its real function. Any similar remote/keypad
  device on a network will hit the same gap. Not something `build`/`applyState` can fix on their
  own - it needs either a new trait for button/scene actions, or excluding a bare `"battery"` expose
  from being sufficient to classify a device as a Sensor by itself.
- **`"state"`'s `"ON"`/`"OFF"` string convention is hardcoded** rather than read per-device from
  each expose's own `value_on`/`value_off` (unlike, say, `nodeValue`'s handling in
  `bridges/zwave`) - confirmed as a fixed convention across every switch/light device on the real
  broker this bridge was checked against (their `"state"` exposes' own `value_on`/`value_off`
  metadata all read exactly `"ON"`/`"OFF"`), so hardcoding it mirrors `bridges/zwave`'s own choice
  to hardcode `"targetValue"`/`"currentValue"` for CC 37. `"power"`/`"current"`/`"voltage"`'s
  hardcoded property names for a metering plug remain unconfirmed - no metering smart plug was
  present on the network this bridge was checked against, only a plain (non-metering) in-wall
  switch and several dimmers.
- **No transactional multi-property writes.** A `Colour` HSB command writes `{"color":
  {"hue":...,"saturation":...}}` as a single MQTT publish (unlike `bridges/zwave`'s RGB builder,
  which issues three independent per-channel writes) - zigbee2mqtt's `/set` topic natively accepts
  a composite JSON object in one message, so there's no equivalent partial-failure risk to guard
  against here.
- **`WriteState`'s echo-wait can spuriously fail on a stale message** - confirmed live, not
  theoretical (see "Verified against a live broker"): a device-info/metadata republish that
  happens to land on the same state topic right after a `/set` publish, but before the real echo,
  reads as "did not accept" even though the device did accept the write. No automatic retry is
  implemented; a caller that hits this today needs to retry the command itself.
- **`Fan.Toggles` discovery is a narrow, hand-picked list (`fanToggleCandidates`)**, not a generic
  "every settable binary expose becomes a toggle" heuristic - only `"led_enable"`/`"child_lock"`
  are recognized today, both confirmed on the one real fan this bridge was checked against (an
  IKEA STARKVIND). A different fan model's LED/lock/oscillation/etc. toggles, under different
  property names, won't be picked up without adding them to that list.
- **`Fan.Speed`'s read-only-telemetry convention (a separate top-level `"fan_speed"` sibling
  property) is confirmed only for the IKEA STARKVIND.** A fan whose "speed" is instead a settable
  numeric feature nested inside the `"fan"` composite itself (a shape z2m's public exposes docs
  describe for some other fan products) isn't modeled at all - such a device would still build (as
  long as it has a `"state"` feature) but with no `Speed` trait, `Mode` populated from its `"mode"`
  feature if present as normal.

## Verified against a live broker

This pass was validated against a real zigbee2mqtt instance managing a live, 24-device home Zigbee
network (base topic `zigbee`, not the default `zigbee2mqtt`) - a genuinely mixed fleet: Enbrighten
in-wall switches/dimmers, several generations of Philips Hue bulbs (colour and colour-temperature-
only), Gledopto RGB(W) LED controllers, a GE bulb, Aqara water-leak sensors, an IKEA door/window
sensor, an IKEA air-quality/temperature/humidity sensor, a Philips Hue motion sensor, a Philips Hue
battery remote, an IKEA STARKVIND air-purifier (fan), and a Tuya garage-door opener (cover) - the
last confirming this bridge's cover-scoping decision against a real device of exactly that type,
not just a theoretical exposes shape.

Confirmed working end-to-end:
- **Discovery**: all 24 `bridge/devices` entries processed without error; the 21 in-scope devices
  (switches, dimmers, colour bulbs, sensors, the fan) built correctly, and the 3 out-of-scope ones
  (Coordinator, the battery remote's controller-only device, and the garage-door opener) were
  skipped cleanly via the documented "no supported exposes" path - not silently, not by crashing.
- **Colour temperature range conversion**: a real Hue colour bulb's `value_min: 153`/
  `value_max: 500` (mireds) built `ColourTemperatureRange{min_k: 2000, max_k: 6536}` - the exact
  `1,000,000/mired` conversion this bridge implements, off real device data.
- **Live sensor state**: a real Hue motion sensor's temperature (20.61°C), battery (62%), and
  illuminance all populated correctly and matched the documented `luxToLightLevel` formula exactly
  (`lux: 48` → `light_level: 16813.412`, i.e. `10000*log10(48)+1`) - and confirmed the
  `illuminance_lux`-over-`illuminance` fix above was necessary and correct (a plausible 48 lux
  reading, not a raw sensor count in the thousands).
- **The real IKEA STARKVIND air purifier, end to end**: discovered and built as `Fan`; a fresh
  live report (requested via zigbee2mqtt's `/get` topic) decoded correctly across every field this
  bridge models, including the mixed bool/string toggle encoding the "Fans" section above
  describes (`led_enable: true` from a raw JSON `true`, `child_lock: false` from the raw string
  `"UNLOCK"`) - both landing as the correct Go `bool` from the same `applyState` pass; and a real
  `OnOff` command round trip (`bridgecli onoff --on` against the fan, already on - a safe no-op)
  completed correctly, which also exercised - and confirmed the fix for - the
  `service/bridge/api.go` `deviceSupportsCommand` gap described in "Behavior notes" above (before
  that fix, this exact call failed with "the device does not support the specified command").
  `Mode`/`Toggle` commands were not exercised live against this device (see "not covered" below).
- **A real command round trip against a plain switch**: `bridgecli onoff --on` then, after a
  pause, `--on=false` against a real Enbrighten in-wall switch, both confirmed against the
  physical device's own state (read directly over MQTT, independent of this bridge). The `--on`
  call succeeded first try in ~180ms. The `--on=false` call's *first* attempt failed with "did not
  accept state" - a real, live occurrence of the exact limitation `WriteState`'s doc comment
  already described: the message that arrived right after the `/set` publish was a stale
  `"state":"ON"` device-info republish (zigbee2mqtt occasionally republishes full state alongside
  unrelated metadata, e.g. after a `last_seen`/linkquality update), not yet the real OFF echo -
  `WriteState` doesn't retry past a non-matching first message, so it surfaced as a command
  failure. A second, immediate retry of the identical command succeeded normally. This is a live
  confirmation that the trade-off documented in `WriteState`'s doc comment is real and not merely
  theoretical - a caller (or a future version of this bridge) that wants to ride through this
  should retry once on a "did not accept" error before treating it as a real rejection.
- **`contact`'s open/closed polarity** (see "Sensors" above) - not just behaviourally, but from a
  real device's own exposes metadata: an IKEA PARASOLL contact sensor's `"contact"` expose
  documents `value_on: false, value_off: true` directly, confirming the inversion this bridge
  applies is correct, not merely a plausible guess.
- **Continuous live updates**: with the bridge left running, further unprompted state updates
  (e.g. the IKEA air-quality sensor's periodic reports) routed and applied correctly on their own,
  not just at initial discovery.
- **State survives an unrelated `bridge/devices` republish**: this was validated by a targeted
  unit test (`TestHandleBridgeDevices_PreservesStateAcrossRediscovery`) reproducing the exact
  defect described in "Behavior notes" above, rather than by re-pairing/renaming a real device on
  the live network mid-session (deliberately avoided, to not disturb anything else on it).

**Not covered by that broker, still unverified:**
- **A state-changing `BrightnessAbsolute`/`BrightnessRelative` or `Colour` command against a light
  or dimmer**. The only light-adjacent commands actually issued were the fan's `OnOff` and the
  switch's `OnOff` (see above); a light's `/set` payload shape for `brightness`/`color`/
  `color_temp` is confirmed only against zigbee2mqtt's own documentation and this network's
  `bridge/devices` exposes, not an actual accepted write.
- **A `Mode` or `Toggle` command against the fan** - only `OnOff` was exercised live (see above);
  `fanBuilder`'s `Mode`/`Toggle` write paths are unit-tested but not confirmed against the real
  STARKVIND's actual `/set` acceptance.
- **A metering smart plug's `power`/`current`/`voltage` property names** - no such device was on
  the test network; see "Known limitations".
- **`color_xy`-only lights, and multi-endpoint devices** - none were present on the test network.
- Everything else already called out in "Known limitations" as an unverified assumption.

## Setting up against a real zigbee2mqtt instance

1. Have zigbee2mqtt running with its MQTT settings pointed at a broker this bridge can also reach.
   Note the configured base topic (`mqtt.base_topic`, `"zigbee2mqtt"` by default) - it goes
   directly into `zigbee.mqtt.base_topic`.
2. Copy `zigbee.example.yaml`, fill in the broker URL/credentials and the base topic from step 1.
3. Run the bridge:

   ```sh
   bazel build //bridges/zigbee
   bazel-bin/bridges/zigbee/zigbee_/zigbee   # run from the directory containing your config
   ```

4. Using `bridgecli` (`bazel build //clients/bridgecli`):

   ```sh
   bridgecli --addr 127.0.0.1:17015 bridge --bridgeID <bridge ID> listDevices
   bridgecli --addr 127.0.0.1:17015 device --deviceID zigbee-<ieee, 0x stripped> onoff --on
   ```

   Confirm the command actually reached the device (not just that the bridge's own optimistic
   cache updated) by watching the physical device or zigbee2mqtt's own frontend/log. Given "Known
   limitations" above, a `contact` sensor's open/closed polarity and any `color_hs` light's actual
   hue/saturation response are the two things most worth double-checking first against real
   hardware.
