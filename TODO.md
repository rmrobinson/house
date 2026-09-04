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
[ ] standing desk sit/stand presets via the existing trait.App (reuse as scene: trait.App) - requires implementing command.App, which no bridge currently wires up despite the trait existing
[ ] trait.Input and trait.App have no command implementation anywhere in the repo yet (command.proto has no Input/App oneof entries) - first device to need either will have to add this