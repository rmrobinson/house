# viewerui

Server-rendered house dashboard (vertical slice): floors → rooms → a room's
live properties, linked devices with `[ ON ]`/`[ OFF ]` toggles, and one
camera's WHEP stream. Same architecture as `../adminui` - `html/template` +
htmx, no JS SPA, no grpc-web - and it shares that UI's building blocks via
`../lib` (`hub`, `houseview`, `htmxutil`, `webassets`) rather than copying
them. Standalone binary/image, separate from adminui.

## Running

```sh
bazel run //service/viewerui   # reads viewerui.yaml from /etc/house, $HOME/.config/house, or .
```

See `viewerui.example.yaml`. `viewerui.house_addr` is a running `housed`;
`viewerui.bridge_facade_addr` defaults to it (set it only if the facade runs
as a separate `bridgefacaded`). Optional mTLS via `viewerui.tls.*`, same keys
as `adminui.tls.*`.

## Routes

| Route | Behavior |
|---|---|
| `GET /` | Redirects to the building's overview if there's exactly one, else lists them |
| `GET /buildings/{id}/overview` | Landing page: house tiles (mode, occupancy, total power, indoor/outdoor temp, leaks), every thermostat's setpoint, per-floor motion |
| `GET /buildings/{id}/overview/events` | SSE for the overview: debounced re-render of the body, plus the exterior-motion camera popup |
| `GET /rooms/{id}/popup` | Camera overlay for a room (every linked camera); 204 if it has none |
| `GET /buildings/{id}` | Floor list + room list for the first floor (`?floor=`, `?room=` preselect) |
| `GET /buildings/{id}/floors/{floor_id}` | htmx swap of the floor/room panel |
| `GET /rooms/{id}` | htmx detail pane: Properties grid, devices, VIEW CAMERA if a Camera is linked (a direct browser hit redirects to the full page) |
| `GET /rooms/{id}/camera/{device_id}` | `<video>` + inline WHEP negotiation against `Camera.media_stream.state.url` |
| `POST /devices/{id}/commands` | `on=true\|false` (OnOff) or `brightness=0-100` (BrightnessAbsolute, the dimmer slider) → `ExecuteCommand` → swaps the device row. Requires the `HX-Request` header (CSRF guard); other values are a 400 |
| `GET /buildings/{id}/events` | SSE: `HouseService.StreamHouseUpdates` (occupancy dots, open room's grid, event log) + `BridgeService.StreamUpdates` (device rows) |

## Overview and exterior-motion popup

- **Exterior** is an explicit flag on a Floor (`Floor.exterior`, set in
  adminui's floor form or `housecli -exterior`), not inferred from names. Motion
  starting (room occupied false -> true) in any room of an exterior floor
  opens the camera overlay on the overview. It stays while any exterior room
  has motion and closes 30s after the last one goes quiet; `[ X ]` closes it.
  It only fires while the overview page is open.
- **Deploy order.** `UpdateFloor` replaces a floor wholesale, so an adminui
  built before `Floor.exterior` existed will silently clear the flag when
  anyone saves a floor. Deploy `housed`, then adminui and viewerui together,
  and don't edit floors in an old adminui.
- **Overview refresh cost.** Any device change re-renders the overview
  (floors + rooms + devices fetched), coalesced to at most one render per tab
  per 5s.
- **`weather_psk` travels in cleartext** unless `weather_tls` is on; put it in
  the secrets file (`!secret`) rather than the yaml.
- **Total power** sums `Room.Properties.power_draw_w`, so it covers metered
  devices only - not the whole-house draw.
- **Climate has Interior and Exterior sub-headings.** Interior is the room
  average temp, average humidity, the worst CO2/VOC/radon room, and each
  thermostat's set point. Exterior is weather-server's newest reading (temp
  and feels-like, conditions, humidity, wind, rain today, UV, pressure). Set
  `viewerui.weather_addr` (plaintext gRPC, or `weather_tls: true` for the nginx
  front on :443, e.g. `weather.ktc1.net.faltung.ca:443`; plus `weather_psk` if the
  server has `WEATHER_PSK`); unset, or a reading older than 15 minutes, shows one
  placeholder tile saying why. The proto is vendored at
  `api/weather/v1/weather.proto` - weather-server stays the source of truth.

## Notes

- **Hardening.** Every response carries `X-Frame-Options: DENY`/CSP
  `frame-ancestors 'none'`/`nosniff`, and non-static responses are
  `no-store`. SSE sends a `: ping` every 20s; the page shows a live/disconnected
  badge and reloads on reconnect (SSE only carries deltas). Request errors show
  in a banner. The event log is capped at 100 lines.

- **Room order is by name.** `Room` has no stored position field, so the
  plan's "ordered by stored position" isn't possible yet.
- **Camera URL must be browser-reachable and WHEP.** The client POSTs an SDP
  offer (`application/sdp`) straight to `media_stream.state.url` from the
  browser, so that URL must be http(s), reachable from the viewer's machine,
  and a WHEP endpoint (e.g. go2rtc). Anything else renders an explanatory
  message instead of a player. No bridge in this repo populates the URL yet,
  so this path is untested against a real stream.
- **Live updates use `sse-swap="message"`** on a hidden element
  (`templates/building.html`): htmx's SSE extension only listens on elements
  carrying it, and each message is a set of `hx-swap-oob` fragments.
- Out of scope this pass: SVG floor map, media/energy/security panels,
  multi-camera grid, PTZ.
