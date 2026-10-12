# TODO

## Sensors
[x] use the Airthings Go API to create a sensor
[ ] integrate with zigbee2mqtt for a device sensor
[ ] integrate with zwave2mqtt for a device sensor
[x] integrate with frigate for a device sensor
[x] integrate with plex for a device sensor
[x] integrate with tesla API for a device sensor
[ ] integrate with LG API for a device sensor
[x] integrate with Roku ECP for a device sensor

## Configuration

[ ] create an LXC container on the devices subnet to monitor for Roku & TV devices
[ ] assign IP to MAC map for LG TV
[ ] assign IP to MAC map for Roku

## Bridges

[ ] add hot-reload config support (fsnotify watch + live ADDED/REMOVED device reconciliation) to the ESPHome bridge, deferred out of the v1 build
[ ] add TLS support to bridge gRPC servers (service/bridge.Server currently serves plaintext for all bridges, including the new ESPHome one)

## Device types

[ ] add api/device/fan.proto (OnOff + existing trait.Speed) and a command.SpeedAbsolute/SpeedRelative pair (mirrors BrightnessAbsolute/BrightnessRelative); wire ExecuteCommand validation in service/bridge/api.go; add a fanBuilder to the ESPHome bridge (maps to ESPHome's Fan entity domain, client.SetFan already exists)
[ ] fan oscillation/direction/preset-mode support - blocked on deciding how a device with multiple Mode/Input-shaped fields disambiguates which field a command targets (same open question as Input/App below); do this deliberately once rather than as a fan-specific hack
[ ] add api/trait/position.proto (new trait: can_control/supports_stop attrs, current_position 0-100 state) + api/device/standing_desk.proto (Position, optional on_off) + command.PositionAbsolute/PositionRelative/Stop; wire ExecuteCommand validation; add a builder to the ESPHome bridge mapping Position <-> ESPHome's Cover entity domain (not Fan/Light - real desk controllers expose as Cover)
[ ] standing desk sit/stand presets via the existing trait.App (reuse as scene: trait.App) - command.AppLaunch now exists (added for the Cast bridge, see bridges/cast/README.md) and is the right shape (a string ID) for this, but no ESPHome builder wires trait.App/AppLaunch to the Cover entity domain a desk uses
[ ] trait.Input has no command implementation anywhere in the repo yet (command.proto has no Input oneof entry) - first device to need it will have to add this (trait.App now has command.AppLaunch, added for the Cast bridge)
[ ] Inovelli VZM31-SN: control the notification LED bar separately from the load. Today the zigbee bridge only exposes the dimmer's load (OnOff/Brightness); the LED bar (zigbee2mqtt `led_effect` / `individual_led_effect`, plus the LED colour/intensity-when-on/off attributes) needs its own addressable surface - e.g. a second set of traits on the same device with an explicit target, rather than overloading the load's Brightness/Colour. Blocked on the same open question as fan presets and trait.Input/App above: how a device with multiple same-shaped fields disambiguates which one a command targets - decide that once. Mind the z2m one-attribute-per-/set gotcha when writing LED settings. The multi-tap `action` button events for the same hardware are already built (Buttons trait, see bridges/zigbee/README.md), so this is only the LED side
[ ] add device types for locks, covers/blinds/shades and garage doors (api/device has none of these today; api/trait/position.proto already exists from the standing desk work and fits covers/blinds, which would reuse Position + command.PositionAbsolute/PositionRelative/Stop). Needs: a lock trait (locked/unlocked/jammed state, can_control attr) + command, a garage-door shape (open/closed/opening/closing, likely Position or a new door trait), api/device protos composed from them, ExecuteCommand validation in service/bridge/api.go's deviceSupportsCommand, and a first bridge to prove it (zigbee/zwave locks and covers via z2m/zwave-js are the obvious source). Locks and garage doors are security-sensitive: decide up front whether unlock/open commands need extra handling (e.g. refuse from policy scripts by default, or require an explicit allow) rather than treating them like OnOff

## Facade

[ ] device aggregation in service/bridge/facade: layer traits from different bridges that expose properties of the same underlying physical device into one Device - e.g. the omada bridge's NetworkPresence/reachability + the device's own bridge (zigbee, cast, roku, ...), or a speaker whose volume/power is controlled through the monoprice receiver while the Google Cast device powering it supplies now-playing/media. Design questions to settle first: (a) how the link is identified - shared MAC/IEEE address in Device.Address, or an explicit house-level link persisted in housedb next to DeviceConfigOverlay (needed for the receiver+cast case, which share no address); (b) which bridge is primary (owns Device.Id/Config.Name) and how conflicting values for the same trait resolve (per-trait precedence vs. error); (c) command routing - ExecuteCommand on the merged device must dispatch each command to the bridge owning that trait, and Device.version/checkVersion has to stay meaningful across more than one upstream; (d) StreamUpdates must emit one merged update rather than N, and a contributing bridge going away must degrade the device, not delete it; (e) adminui/viewerui/policyd all consume the facade through housed, so the merged device should be transparent to them with no per-client change

## Metrics

[ ] store device state changes, commands and Airthings history in InfluxDB v2 via a new service/metrics daemon (metricsd): CommandEvent on the bridge update stream + Command.origin, deadband+heartbeat sampling (all params configurable), optional BridgeService.GetDeviceHistory (Airthings first; port the telegraf RefreshHistory logic), daily backfill scheduler, Airthings refresh_interval 300s -> 21600s, 30d raw / ~548d downsampled buckets. Full phased plan: house-metrics-plan.md (handoff doc, ~/Downloads)

## Policy engine

[ ] scenes: a named, house-level snapshot of target device states applied with one call. Only per-device `Light.scene` (trait.App + command.AppLaunch) exists today, used by the nanoleaf and zigbee bridges; there is no cross-device scene. Open questions: where scenes live (housedb next to rooms vs. policy store), whether a scene targets explicit devices or room/floor selectors (ties to the area-targeted queries item), how apply handles a device that is unreachable or lacks the trait, and how it is triggered (a policy script binding like home.applyScene, an adminui/viewerui button, or both)
[ ] combined time-trigger item: add the missing time condition types alongside schedule.daily (which already has Weekdays + TZ), date-range, daylight, sun-event, event.idle-for and held-for in service/policy/attribute_conditions.go. (a) interval trigger ("every N minutes") and cron-expression trigger; (b) calendar trigger fed by a local source (CalDAV or an ICS file/URL on the LAN - no cloud calendars) so events like "guests arriving" can drive a condition. Decide whether cron subsumes daily or they coexist, and how calendar events surface as a momentary pulse vs. a held-true window. Also register any new type in the adminui policy editor's help text (it currently documents and/or/not but not held-for, though the engine parses it - policy.go "held-for" / DurationMS)
[ ] person presence: there is no person entity (api/trait has Presence for motion/occupancy and NetworkPresence for devices, but nothing models a person). Add a person concept whose presence is derived locally, e.g. from NetworkPresence of the devices assigned to them (omada), with an "everyone away" / "anyone home" derived state usable as a policy condition and a house-state input. Open questions: where persons are stored (housedb), how a person is associated with devices, debounce/grace period before flipping to away, and how this interacts with the existing sys.occupied (room-motion based) and house mode
[ ] area-targeted queries for policies: home.findDevices(kind) in service/policy/bindings.go filters by device kind only, and HomeAPI.GetDeviceRoom maps one device to its room. Add a way for a script and for devices.any-match (device_conditions.go) to select devices by room, floor or building (e.g. "all lights on the main floor"), backed by HouseService.ListDeviceLinks / the room hierarchy, which already holds the data but is not exposed to policies. Shares its selector design with the scenes item

## Integrations

[ ] MQTT output: publish house device/state changes to a local MQTT broker so other LAN tools can consume them. Today MQTT is only consumed (the zigbee and zwave bridges talk to their own brokers for commands/state); nothing publishes house state outbound. Likely a small standalone consumer service in service/<name> that follows bridgeconn's StreamUpdates (same shape as service/policy/bridgehome and the planned metricsd), with topic layout, retained-message and QoS choices, and optional per-room/device filtering to decide up front. Avoid a feedback loop with the zigbee/zwave bridges if both point at one broker

## Platform

[ ] backup and restore: there is no backup tooling in the repo. State that would need protecting is the house SQLite DB (service/house/db: buildings, floors, rooms, device-room links, device config overlay/renames, building state) and the policy SQLite store (service/policy/store.go: user policies, execution logs), plus the YAML configs (notification recipients live in config, not a DB) and the generated bridge.id values each bridge persists. Wants a consistent online snapshot (SQLite `VACUUM INTO` or the backup API, not a raw file copy of a live WAL DB), a single command/endpoint to produce a bundle and one to restore it, and a decision on where the bundle goes (local path/NAS only - no cloud). Restore must be safe against the migrations in service/house/db (never restore a newer-schema DB into an older binary) and should note that policyd's persisted sys.* policy copies are what's live, not the source scripts
[ ] device and bridge diagnostics: is_reachable and last_seen are already exposed (adminui shows Online; the battery-report policy uses last_seen and reachability), but there is no single view of device/bridge health. Add: signal strength (RSSI/LQI/link quality) as a field bridges can report where the protocol provides it (zigbee LQI, zwave, BLE RSSI for airthings, wifi RSSI for esphome), per-bridge connection health and last-successful-refresh time (so a wedged bridge is distinguishable from a quiet one), and an adminui diagnostics page listing unreachable/stale/low-signal devices. Decide whether bridge health is a new field on api.Bridge or derived by the facade from its upstream connection state
