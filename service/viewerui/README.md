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
| `GET /devices/{id}/sparklines` | Charts for the device's numeric readings over `?range=24h\|7d\|30d` (default 24h); 404 unless `metrics_addr` is set, 400 for another range |
| `GET /buildings/{id}/overview/trends` | The overview's TRENDS box: whole-house charts over `?range=24h\|7d\|30d` (default 24h); 404 unless `metrics_addr` is set, 400 for another range |
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
  front on :443, verified against
  `weather_ca_file` or the system roots - its cert is from the house CA, so
  point it at `tls.ca_file`; plus `weather_psk` if the
  server has `WEATHER_PSK`); unset, or a reading older than 15 minutes, shows one
  placeholder tile saying why. The proto is vendored at
  `api/weather/v1/weather.proto` - weather-server stays the source of truth.

## Reading history (sparklines)

Set `viewerui.metrics_addr` to metricsd's `MetricsService` (its `listen_port`,
17050 by default) and each device with numeric readings gets a third row under
its readings row: one small chart per reading, with a 24h / 7d / 30d toggle.
Unset, the row isn't rendered and `/devices/{id}/sparklines` is a 404.

- **Which readings.** Only numeric ones (`houseview.Reading.Metric` is set:
  temperature, humidity, CO2, power, battery %, ...); text and bool readings
  have none. The metric to read is derived from the device on the server, never
  from the request.
- **Lazy and cached.** The row loads once when it scrolls into view
  (`hx-trigger="intersect once"`, so a hidden DETAILS row costs nothing), one
  request per device. Each `(device, metric, range)` is cached for 60s; failed
  lookups are not cached. A chart whose history can't be read says so rather
  than failing the row.
- **Not in the live push.** The SSE push swaps only the device row and readings
  cell, so a push never resets the range you picked.
- **Time axis.** Each chart spans the whole window, so a metric recorded for
  only a few hours sits at the right edge instead of stretching to fill it. 30d
  reads the hourly rollup, which lags by about an hour and only reaches back to
  when the rollup began.
- **TLS.** Dialed with the same mTLS identity as the house dial.
  `metrics_server_name` overrides the name checked against metricsd's cert for
  this connection (`tls.server_name` names housed); empty uses the host in
  `metrics_addr`, which on h031 means mapping `metricsd.h031.house.internal` to
  loopback via `extra_hosts`, as for housed.

### Overview trends

With `metrics_addr` set, the overview gets a TRENDS box between the CLIMATE and
MEDIA boxes: interior temperature and humidity (mean), CO2 / VOC / radon (worst
sensor), house power, and one power chart per EV charger, with the same 24h / 7d /
30d toggle.

- **Rebuilt, not read.** housed's room aggregation keeps no history, so each
  chart is built from the member devices' recorded series, merged by metricsd
  (`GetSeries` `device_ids` + `combine`: MEAN, MAX or SUM). It follows the same
  devices as its tile but not housed's per-room strategies, so the two are close
  rather than identical; the box says so.
- **Which devices.** Derived from the live snapshot on the server, never from
  the request. Climate charts use devices outside exterior floors that report
  the metric; house power sums every device recording `power.power_w`,
  `wall_power.power_w` or (for a sensor with no Power trait)
  `power_consumption.power_usage_w`. A SUM splits across requests past
  metricsd's 64-device cap; a MEAN is capped at 64. Temperature only covers
  sensors that record `air_properties.temperature_c`.
- **Not in the live push.** The overview body is two swap targets
  (`#overview`, `#overview-rest`) with TRENDS between them, so the SSE
  re-render never resets the charts or the chosen range. The box loads on page
  load and is cached per series for 60s.
- **Gaps.** A SUM understates a window where a device has no point, and a chart
  whose history can't be read shows "history unavailable" instead of a partial
  total.

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
