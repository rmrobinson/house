# nanoleaf bridge

Bridges one or more Nanoleaf Light Panels controllers into the `house` ecosystem, via
[`nanoleaf-go`](https://github.com/rmrobinson/nanoleaf-go).

## Architecture

Like `bridges/esphome` (and unlike `bridges/zwave`'s single shared MQTT connection), **each
configured panel gets its own persistent connection** - see `panel.go`'s `panelConn`. A Nanoleaf
controller is an independent HTTP+SSE endpoint on its own IP; there's no shared gateway to
multiplex the way zwave-js-ui's MQTT broker lets `bridges/zwave` get away with one connection for
an entire network.

`panelConn.run` seeds the initial house device from one `GetPanel`/`GetEffects` call pair
(retrying on a fixed delay if the panel is unreachable at startup), then calls
`nanoleaf.Client.SubscribeEvents` for the panel's `state`(1) and `effects`(3) event types and
applies every update it delivers for the lifetime of the connection. Reconnect/backoff for that
event stream is entirely nanoleaf-go's own responsibility (`stream.go`'s `SubscribeEvents`) - this
bridge never talks SSE directly. `layout`(2) and `touch`(4) events are not subscribed to: no trait
here models panel geometry or touch gestures.

There's no `Refresh` polling loop (same as esphome/zwave) - state arrives entirely over the event
stream once seeded.

## Device → trait mapping (`Light`)

- `OnOff` ← panel `state.on`.
- `Brightness` ← panel `state.brightness` (already a 0-100 scale - no rescaling needed, unlike
  `bridges/zwave`'s 0-99 Multilevel Switch levels).
- `Colour`: `Attributes.Mode` is always `MODE_HSB` - Nanoleaf has no RGB endpoint, only hue/
  saturation and colour temperature. `Attributes.ColourTemperatureRange` comes from the panel's
  own reported `state.ct` min/max. `State.Hsb` carries hue/saturation only - brightness is driven
  exclusively through the `Brightness` trait, matching how `bridges/zwave`'s RGB builder keeps
  brightness and colour writes independent. A `command.Colour.Rgb` command is rejected
  (`bridge.ErrUnsupportedCommand`) rather than approximated, since this device genuinely can't
  take one. Hue/saturation and colour-temperature-K state are both always populated regardless of
  which one is actually driving the panel's current output - the panel's own `state.colorMode`
  (`"hs"`/`"ct"`/`"effect"`) isn't surfaced, so a client can't tell from this trait alone which
  representation is currently authoritative. See "Known limitations" below.
- `Scene` (the `App` trait): `Attributes.Applications` is the panel's effect catalog from
  `GetEffects` (`Instance{Id: name, Name: name}` - Nanoleaf effects are keyed by name, there's no
  separate ID). `State.ApplicationId` is the currently active effect. `command.AppLaunch` selects
  an effect by name (`SetScene`); effect *creation* isn't supported anywhere in this stack
  (nanoleaf-go doesn't implement it either).

## Pairing

`cmd/pair` prints a new API key for a panel: hold its power button 5-7s until the LEDs flash
(opens Nanoleaf's own ~30s pairing window), then run it (or run it first and press the button
while it polls - see `--timeout`/`--poll`). Paste the printed key into the matching device's
`api_key` field in `nanoleaf.example.yaml`. Unlike `bridges/ecobee`'s HomeKit pairing, there's no
persistent store - an API key is a single opaque string, handled the same way `bridges/esphome`'s
`noise_psk` is.

## Known limitations

- **`state.colorMode` isn't tracked.** During an active effect, the panel's hue/saturation/CT
  fields may not reflect what's actually being displayed - Nanoleaf doesn't stop reporting them,
  it just stops them being authoritative. A client watching the `Colour` trait during an effect
  will see stale-looking values rather than an indication that colour is currently effect-driven.
- **The effect catalog (`Scene.Attributes.Applications`) is fetched once, at connect time.** A
  effect created or deleted via the Nanoleaf app afterwards won't appear/disappear until the next
  reconnect. `Scene.State.ApplicationId` itself does stay live (it comes from the `effects`(3)
  event, not the catalog fetch).
- **Nanoleaf's rate limit isn't specifically handled.** nanoleaf-go's `ErrTooManyRequests` (HTTP
  429) surfaces like any other client error - a command hitting it fails with `codes.Internal`
  rather than being retried. Hasn't been observed in practice against a single panel driven at
  normal command rates.

## Setting up against a real panel

1. Find the panel's address if you don't already know it - it advertises itself over mDNS as
   `_nanoleafapi._tcp` (port defaults to 16021). On macOS: `dns-sd -B _nanoleafapi._tcp` to find
   the instance name, then `dns-sd -L "<instance name>" _nanoleafapi._tcp local.` to get its
   hostname:port. On Linux: `avahi-browse -r _nanoleafapi._tcp`. `bazel build
   //bridges/nanoleaf/cmd/pair` and run it against that IP, pressing the pairing button as
   described above under "Pairing".
2. Copy `nanoleaf.example.yaml`, filling in each panel's `host` and the API key from step 1.
3. Run the bridge:

   ```sh
   bazel build //bridges/nanoleaf
   bazel-bin/bridges/nanoleaf/nanoleaf_/nanoleaf   # run from the directory containing your config
   ```

4. Using `bridgecli` (`bazel build //clients/bridgecli`):

   ```sh
   bridgecli --addr 127.0.0.1:17015 bridge --bridgeID <bridge ID> listDevices
   bridgecli --addr 127.0.0.1:17015 device --deviceID nanoleaf-<id> onoff --on
   ```

   Confirm commands and live state changes (toggling the panel physically or via the Nanoleaf app)
   both show up correctly, not just that the bridge's own optimistic cache updated.

## Verified against a live panel

Tested end-to-end against a real Nanoleaf Light Panels controller (model `NL22`, firmware
`5.3.2`), found via mDNS as described above. Confirmed working:

- **Discovery**: `dns-sd -B`/`-L`/`-G v4` correctly located the panel's instance, hostname/port
  (`:16021`), and resolved IPv4 address.
- **Pairing**: `cmd/pair` correctly polled through a 401 (button not yet pressed) and returned a
  working API key once the panel's pairing button was pressed.
- **Initial seed**: `GetPanel`/`GetEffects` correctly built the device on connect, including the
  full 21-entry real effect catalog and the panel's actual reported state.
- **Commands**: `OnOff`, `BrightnessAbsolute`, and `AppLaunch` (effect selection) all round-tripped
  correctly via `bridgecli` - the panel visibly responded, and the returned `Device` reflected the
  new state.
- **Live event-stream updates**: confirmed independently of the command path - after each command,
  a separate `effects`(3)/`state`(1) event arrived over the SSE stream shortly after (~150ms-1s)
  and matched what had already been applied optimistically (logged as "skipping update since
  device hasn't changed", `service/bridge/service.go`'s dedup working as intended). This confirms
  nanoleaf-go's `SubscribeEvents` is genuinely connected and receiving live updates from the
  device, not just idle.
- **A real, non-nanoleaf-specific bug was found and fixed by this pass**: `service/bridge/api.go`'s
  `deviceSupportsCommand` had no case for `Light.Scene` + `AppLaunch` (only `MediaPlayer`/
  `Television`'s `App` field were wired up), so every effect-selection command was rejected with
  `ErrCommandNotSupported` before ever reaching this bridge. Fixed by adding the missing
  `d.GetLight().GetScene() != nil && req.GetAppLaunch() != nil` case, with matching test coverage
  in `service/bridge/api_test.go`. This affects any bridge putting an `App` trait on a `Light`
  device, not just this one.

**Not covered by this pass**: `BrightnessRelative`, `Colour` (HSB/CT) commands, `RGB`-rejection,
and multi-panel behaviour (only one physical panel was available to test against) - these are
covered by the fake-client unit tests in `devices_test.go` but not confirmed against real
hardware. The `SetHue`/`SetSaturation`/`SetCT` calls follow the identical wire pattern (a JSON PUT
to `state`) as the now-verified `SetOn`/`SetBrightness` calls, via the same nanoleaf-go client, so
the remaining risk is low - but unconfirmed. `bridgecli` itself has no `colour` subcommand today,
so exercising this further would need either a CLI addition or a raw gRPC call.
