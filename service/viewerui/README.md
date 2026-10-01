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
| `GET /` | Redirects to the building if there's exactly one, else lists them |
| `GET /buildings/{id}` | Floor list + room list for the first floor (`?floor=`, `?room=` preselect) |
| `GET /buildings/{id}/floors/{floor_id}` | htmx swap of the floor/room panel |
| `GET /rooms/{id}` | htmx detail pane: Properties grid, devices, VIEW CAMERA if a Camera is linked (a direct browser hit redirects to the full page) |
| `GET /rooms/{id}/camera/{device_id}` | `<video>` + inline WHEP negotiation against `Camera.media_stream.state.url` |
| `POST /devices/{id}/commands` | `on=true\|false` (OnOff) or `brightness=0-100` (BrightnessAbsolute, the dimmer slider) → `ExecuteCommand` → swaps the device row. Requires the `HX-Request` header (CSRF guard); other values are a 400 |
| `GET /buildings/{id}/events` | SSE: `HouseService.StreamHouseUpdates` (occupancy dots, open room's grid, event log) + `BridgeService.StreamUpdates` (device rows) |

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
