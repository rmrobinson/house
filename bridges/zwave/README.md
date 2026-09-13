# zwave bridge

Bridges a Z-Wave network into the `house` ecosystem via [zwave-js-ui](https://github.com/zwave-js/zwave-js-ui)
(formerly `zwavejs2mqtt`), talking to its MQTT gateway rather than the Z-Wave radio directly.

## Architecture

Unlike `bridges/esphome` (one connection per physical node), this bridge owns exactly one MQTT
connection shared by the entire Z-Wave network - zwave-js-ui already multiplexes every node's
value topics onto that one broker connection, so there's no per-node dial step. See `mqtt.go` for
the connection/value-topic/`WriteValue`-with-echo primitives and `network.go` for node discovery
and value-topic routing.

Node discovery is automatic: on connect (and every reconnect), the bridge calls zwave-js-ui's
`getNodes` API once and builds a house device for every node whose Z-Wave *generic device class*
it recognizes (see "Supported device types" below). Unlike ESPHome's per-node `roles:` config,
there's no manual entity-to-role mapping needed - Z-Wave command-class/property naming is
standardized enough that the role-to-value mapping comes from a fixed convention table keyed by
generic device class. Config (`zwave.example.yaml`) only needs a handful of per-product-model
overrides for the things the standard can't infer - see "Config overrides" below. Each discovery
pass also prunes any previously-built device whose node is missing from the fresh `getNodes`
response (excluded, factory-reset, or replaced hardware) - see `networkConn.pruneRemovedNodes` -
so a removed node doesn't linger as a permanent unreachable "ghost" device.

**This bridge only supports zwave-js-ui's "named topics" MQTT scheme** - the default, and the only
one it targets. An earlier version of this bridge was built around zwave-js-ui's alternative
numeric-topic-id scheme (`<prefix>/<nodeId>/<commandClass>/<endpoint>/<property>`) without ever
validating that assumption against a real broker; against a real instance, topics actually look
like `<prefix>/[<location>/]<nodeName-or-nodeID_N>/<ccTopicName>/endpoint_<N>/<property>` (spaces
in multi-word property/propertyKey names replaced with underscores), e.g.
`zwave/second_floor/guest_bedroom/motion_sensor/sensor_multilevel/endpoint_0/Air_temperature`. See
`nodeTopicBase` (devices.go) and `mqttConn.topicFor`/`ccTopicName` (mqtt.go) for how this is built.
There is no numeric-topic-mode support and none is planned - switching zwave-js-ui to that mode is
not required for anything in this bridge.

`ccTopicName` resolves a command class to its topic segment via `defaultCCTopicNames`
(mqtt.go). `sensor_multilevel`/`notification`/`sensor_binary`/`battery` are confirmed against a
real zwave-js-ui v9.1.1 broker (see "Verified against a live broker" below); `color` is taken
directly from zwave-js-ui's own source (`api/lib/Constants.ts`'s `_commandClassMap[0x33] =
'color'`), not guessed; `switch_binary`/`switch_multilevel` are this bridge's best guess, following
the same `"<measurement>_<binary|multilevel>"` pattern the confirmed ones share - override via
`zwave.mqtt.cc_topic_names` if a real switch/dimmer proves one wrong (see "Config overrides").

## Supported device types

- **Binary switches** (CC 37) → `Generic`/`OnOff`. There's no dedicated `Switch`/`Outlet` device
  type in this repo yet, so a wall switch and a smart plug both land on `Generic` - see
  `classifyBinarySwitchLabel` for the (purely cosmetic, `ModelDescription`-only) plug/switch
  disambiguation.
- **Multilevel switches / dimmers** (CC 38) → `Light`/`OnOff`+`Brightness`.
- **RGB(W) bulbs** (CC 38 + CC 51) → `Light`/`OnOff`+`Brightness`+`Colour`. **Requires a
  `colour_channels` config override** - see "Known limitations" below.
- **Sensors** (generic class Sensor Notification `0x07`, Multilevel Sensor `0x21`, or Binary Sensor
  `0x20`) → `Sensor`, with whichever of `Presence`/`AirProperties`/`LightLevel`/`Battery` the
  node's actual command classes support. An earlier version of this bridge only recognized `0x07`
  ("Sensor Notification", the generic class the Z-Wave specification documents for this device
  category) - a real AEON MultiSensor 6, recon'd against a live broker, actually reports `0x21`
  (Multilevel Sensor), and would have been silently skipped entirely. `Presence` is always asserted
  (the one base capability all three generic classes imply); the others are only populated when the
  corresponding CC 49/128 values are actually present in the node's `getNodes` response - a plain
  motion-only sensor won't get an `AirProperties` trait just because it shares a generic class with
  a 6-in-1 multisensor. Motion itself is read from either Notification CC (113,
  `"Home Security"`/`"Motion sensor status"` - preferred, and the variant verified against real
  hardware) or Binary Sensor CC (48/0x30, a flat `"Motion"` boolean) depending on which one a given
  node actually reports - see `findMotionValue`. For the Notification CC case, the initial state
  (at discovery time) trusts the node's own per-value `states` list over a fixed numeric code where
  one is available (`motionDetectedFromNotification`) - that list is `[{"text":"idle","value":0},
  {"text":"Motion detection","value":8}]` (confirmed against a real broker; an earlier version of
  this bridge assumed it was a `{"<code>":"<label>"}` object, which isn't valid JSON for an array
  and aborted the entire discovery pass, not just this one node's label lookup); a live update
  arriving after that has no label to consult (zwave-js-ui's value-topic payload is just the bare
  number) and falls back to the same 0-is-idle/nonzero-is-detected rule the label check itself
  defaults to.

Out of scope for this pass: locks, thermostats, associations/groups, scenes.

## Config overrides

`zwave.mqtt.cc_topic_names` overrides `defaultCCTopicNames` (mqtt.go), keyed by command class
number as a string, e.g. `{"37": "switch_binary"}`. Needed only if a real switch/dimmer/RGB bulb
proves one of the best-guess defaults wrong - see "Architecture" above.

The rest of this section - `zwave.devices` - is per-product-model, not per-bridge. See
`zwave.example.yaml`. Every entry under `zwave.devices` is keyed by `device_id`
(`<manufacturerId>-<productId>-<producttype>`, stable per physical product model - not per node),
and every field is optional. **`device_id` must be zwave-js-ui's decimal `deviceId` field (e.g.
`"271-4098-2049"`), not its hex `hexId` field (`"0x010f-0x1002-0x0801"`)** - `nodeInfo.DeviceID`
(network.go) parses `getNodes`'/nodeinfo's `deviceId` JSON field directly, and `overrideFor` does
an exact string match against it, so a hex-formatted key can never match a real node. Read the
value off a node's own `deviceId` field via zwave-js-ui's node inspector or a raw `getNodes` dump -
don't hand-convert from a datasheet's hex manufacturer/product IDs.

- `id` - override the auto-assigned house device ID (`zwave-<nodeId>`). **Only ever set this to a
  value unique to one physical node.** Two nodes resolving to the same `id` (e.g. the same
  `device_id` override applied to two identical plugs) is detected and rejected - the second node
  logs an error and is skipped rather than being built, so it doesn't silently cross-wire its
  state onto the first node's device.
- `binary_switch_label` - override the plug/switch keyword-match heuristic for a specific product
  it gets wrong. Cosmetic only (`ModelDescription`); doesn't change how the device is controlled.
- `colour_channels` - **required** for any RGB(W) bulb to build at all. Maps `"red"`/`"green"`/
  `"blue"` to that product's actual Color Switch (CC 51) `propertyKey`s.
- `colour_target_property`/`colour_current_property` - override CC 51's write/read property names
  (`"targetColor"`/`"currentColor"` by default). This default is as much of an unverified guess as
  the `colour_channels` propertyKey numbers - it just has a working fallback instead of failing
  closed, since some property name has to be assumed to build a role table at all. If colour
  commands against a bulb consistently time out despite a correct `colour_channels` mapping, this
  is the next thing to check.

## Behavior notes

- **Concurrency is per-device, not per-bridge.** Each discovered device has its own lock, held for
  the duration of a command against it; a slow or unresponsive write against one device does not
  delay state-update processing for any other device on the network. (An earlier version of this
  bridge shared one lock across the whole network for this, which both stalled unrelated devices
  during any single command and - because a command's own write echo re-enters the same code path
  that lock guards - could deadlock outright; see `network.go`/`mqtt.go`'s lock-related doc
  comments if you're touching this code.)
- **A plain "turn on" command against a dimmer or RGB bulb restores its last known nonzero
  brightness**, rather than always jumping to full brightness - mirrors the behavior most Z-Wave
  controllers and physical devices default to. This is bridge-side bookkeeping
  (`builtDevice.lastNonZeroLevel`), not a Z-Wave "restore previous value" wire sentinel, so it only
  has anything to restore to once this bridge process has itself seen or commanded the device to a
  nonzero level at least once; before that, a plain "on" still falls back to full brightness. This
  bookkeeping (`builtDevice.lastNonZeroLevel`) survives a broker reconnect's rediscovery pass too -
  `networkConn.buildNode` carries it forward from the `*builtDevice` being replaced when the same
  node is rebuilt, rather than resetting it to zero.
- **Colour command channels are clamped to 0-255** (`clampColourChannel`) before being written -
  `command.Colour.RGB`'s fields are documented as 0-255 but nothing upstream of this bridge
  actually enforces that range, so an out-of-range value from a caller is clamped rather than
  forwarded as-is to a CC 51 property a real device may not validate itself.
- **`Device.version` is populated** (`version.go`'s `computeVersion`, mirroring
  `bridges/cast`/`bridges/webos`'s `ComputeVersion`), hashing every field a command targets
  (`OnOff`/`Brightness`/`Colour` state, plus `Device.Config`) so `service/bridge/api.go`'s
  `checkVersion` can actually reject a stale `Command.version` instead of every zwave device
  reporting an empty version forever. `Sensor` devices hash to a constant version - every trait a
  Sensor can carry is read-only (`sensorBuilder.applyCommand` always rejects), so there are no
  command-target fields to include; `Address`/`LastSeen` and other pure telemetry are excluded
  everywhere, per the field's documented contract.

## Known limitations

- **RGB(W) bulbs require live-device recon before they can be controlled.** CC 51's propertyKey
  numbering for color components (commonly 0-4 for warmWhite/coldWhite/red/green/blue, but not
  fixed across devices - some bulbs omit the white channels or order them differently) is *not*
  assumed from a datasheet: `rgbLightBuilder.build` fails outright, with an explanatory error, for
  any bulb whose `device_id` has no `colour_channels` override configured. Confirm the actual
  numbering against the bulb's own `nodeinfo`/valueId list (zwave-js-ui's node inspector, or a raw
  `getNodes` dump) before configuring it. The property *names* themselves
  (`colour_target_property`/`colour_current_property`) carry the same unverified-assumption risk,
  just with a working default and an override rather than a hard failure - see "Config overrides".
- **Colour writes are not transactional.** Setting an RGB value issues three independent
  `WriteValue` calls (red/green/blue) in parallel; a partial failure (e.g. 2 of 3 channels
  accepted) is possible and isn't rolled back. Physical Z-Wave devices don't support transactional
  multi-CC writes - this mirrors the protocol's own multicast semantics, not a gap in this bridge.
- **No `Switch`/`Outlet` device type.** A binary switch always becomes `Generic`/`OnOff` regardless
  of whether it drives a light fixture or a plug; `binary_switch_label` only affects the cosmetic
  `ModelDescription`, not the trait shape.
- **A dead or never-interviewed RGB bulb is indistinguishable from a plain dimmer** until it
  completes its first Z-Wave interview: bulb-vs-dimmer classification depends on the node's
  `getNodes` response actually reporting a CC 51 value. Same caveat applies to which optional
  sensor traits (and which motion CC) a `Sensor Notification` node gets - see
  `networkConn.classify`'s and `findMotionValue`'s doc comments.
- **A node that rejects a write doesn't get its actual value re-synced immediately.** When
  `WriteValue`'s echo doesn't match the requested value (the node declined it), the bridge's cached
  device state simply isn't updated to that rejected value - it stays at its last-known-good value
  until the node's next independent report. This is a deliberate trade-off (see `mqtt.go`'s
  `dispatch` doc comment) to keep a command's own echo from re-entering the per-device lock that
  same command already holds.
- **`property`/`propertyKey` in a `getNodes` response aren't always JSON strings.** Every
  Configuration CC (112) value in a node's cached value list - present regardless of whether this
  bridge does anything with CC 112 - encodes them as bare JSON numbers instead (e.g. a parameter
  index), confirmed against a real broker. `nodeValue.UnmarshalJSON` decodes both encodings into a
  canonical string; a strict string-typed field decode (an earlier version of this bridge) fails
  outright the instant it hits one of these, which aborts `getNodes` - and so all discovery -
  entirely, not just parsing of the one CC 112 value this bridge never reads. Given how much of
  this bridge's *own* assumptions this one broker connection already disproved, it's plausible some
  other CC's value shape holds a similar surprise that simply wasn't exercised by the one real
  network available for this pass - treat any future "why did discovery just stop working" report
  as plausibly another instance of this same class of bug before assuming it's something else.

## Verified against a live broker

This pass was validated against a real zwave-js-ui v9.1.1 instance (a controller node, an AEON
MultiSensor 6, and a Zooz outdoor smart plug that happened to be offline/"Dead" at the time).
Confirmed working end-to-end: `getNodes` discovery (including the generic-class and
`states`/`property` fixes above), the sensor's `Presence`/`AirProperties`/`LightLevel`/`Battery`
traits populating from real cached values *and* updating live off the standing MQTT subscription
(a real motion event was observed flipping `Presence` mid-session), the dead plug classifying and
reporting `IsReachable: false` correctly, and `WriteValue` against that dead plug correctly timing
out (`ErrCommandTimeout`) rather than hanging or falsely succeeding.

**Not covered by that broker, still unverified:**
- A real switch/dimmer/RGB bulb actually *accepting* a write - the only real switch on the network
  was offline, so `switch_binary`'s topic name (and `switch_multilevel` for a dimmer, which didn't
  exist on this network at all) could still be wrong; a wrong topic name fails safe (the write
  always times out, indistinguishable from "device didn't accept it") but wouldn't be caught until
  a live write against a *reachable* device is tried. `color`'s topic name is not in this
  "unverified" bucket - it's taken directly from zwave-js-ui's own source rather than guessed (see
  "Architecture" above), so it doesn't carry the same risk as the switch/dimmer guesses.
- Everything already called out in "Known limitations" above as unverified independently of this
  broker: CC 51's `colour_channels`/`colour_target_property`/`colour_current_property` values for
  any specific RGB(W) product.

## Setting up against a real zwave-js-ui instance

1. Have zwave-js-ui running with its MQTT gateway enabled (Settings → MQTT), pointed at a broker
   this bridge can also reach. Note the configured topic prefix (`mqtt.prefix`) and gateway name
   (Settings → Gateway) - both go directly into `zwave.mqtt.prefix`/`zwave.mqtt.gateway_name`.
2. Copy `zwave.example.yaml`, fill in the broker URL/credentials and the two values from step 1.
3. For any RGB(W) bulb on the network, look up its actual CC 51 propertyKey numbering (zwave-js-ui's
   node inspector, or a raw `getNodes` API call) and add a `colour_channels` override - see "Known
   limitations" above. Skipping this means that bulb simply won't build (logged error, not a
   crash).
4. Run the bridge:

   ```sh
   bazel build //bridges/zwave
   bazel-bin/bridges/zwave/zwave_/zwave   # run from the directory containing your config
   ```

5. Using `bridgecli` (`bazel build //clients/bridgecli`):

   ```sh
   bridgecli --addr 127.0.0.1:17014 bridge --bridgeID <bridge ID> listDevices
   bridgecli --addr 127.0.0.1:17014 device --deviceID zwave-<nodeId> onoff --on
   ```

   Confirm the command actually reached the device (not just that the bridge's own optimistic
   cache updated) by watching the physical device or zwave-js-ui's own log/node inspector.
