# adminui

Server-rendered admin UI for house/floor/room topology, device-to-room
linking, and the policy engine's policies/execution log. `html/template` +
[htmx](https://htmx.org) for partial re-renders - no JS SPA framework, no
grpc-web. htmx, its [SSE extension](https://htmx.org/extensions/sse/), and
CodeMirror (the policy script editor) are vendored under `static/`, not
pulled from a CDN, so the app has no runtime dependency on outside network
access.

This is the only UI in the repo - `housed` and `policyd` each expose a gRPC
API (`HouseService`/`BridgeService`, `PolicyService`) and nothing else;
`service/policy/http.go` used to be policyd's own UI but was moved here (see
`handlers_policy.go`/`policy_view.go`/`policy_hub.go`) so both domains are
driven the same way, from one binary.

See `../admin-ui-implementation.md` for the design this implements, and
`../admin-ui-api-changes.md` for the HouseService/BridgeService API it's
built on.

## Running

```sh
bazel run //adminui -- # reads adminui.yaml from /etc/house, $HOME/.config/house, or .
```

See `adminui.example.yaml` for the config shape. `adminui.house_addr` should
point at a running `housed` (`service/house/cmd/housed`); if that `housed`
doesn't embed a BridgeService facade itself, set `adminui.bridge_facade_addr`
to a separately-run `bridgefacaded` instead. `adminui.policy_addr` is
optional - point it at a running `policyd` (`service/policy/cmd/policyd`)
for the `/policies`/`/logs` pages; leave it unset to run without them.

The `/devices` Rename action only works when `housed` embeds the
BridgeService facade itself: housed registers a `house.DeviceConfigOverlay`
in front of its embedded facade (`service/house/deviceconfig.go`) that
persists a renamed device's name to housedb and layers it over every device
read, since no bridge in this repo implements `UpdateDeviceConfig` itself.
Pointing `adminui.bridge_facade_addr` at a standalone `bridgefacaded`
bypasses housed - and that overlay - entirely, so Rename fails against the
raw facade in that topology.

## Pages

| Route | Purpose |
|---|---|
| `/buildings` | List + create buildings |
| `/buildings/{id}` | List + create floors in a building; delete building |
| `/floors/{id}` | Edit name/sort_order; list + create rooms; delete floor |
| `/rooms/{id}` | Linked devices, unlink, add-device picker; delete room |
| `/devices` | All devices sorted by name, combinable Unlinked/Connected filters (`?unlinked=1&connected=1`, kept across a link/move), link/move picker |
| `/policies` | List policies with live status |
| `/policies/new`, `/policies/{id}/edit` | Create/edit a policy (condition JSON + Lua script) |
| `/policies/{id}` | Detail, live simulate, recent executions, unregister |
| `/logs` | Execution log, optionally filtered by `?policy=<id>` |
| `/settings` | Edit house_addr/bridge_facade_addr/policy_addr live (see below) |

Top-level navigation between pages is a plain `<a href>` (full page load);
only in-page actions (create/update/delete/link/unlink, and the two pickers)
go through htmx and swap `#page-content` in place.

## Design notes

- **Single editor, no conflict UI.** Every page reads fresh state on each
  request; `Update*` RPCs still carry `version` so a concurrent edit is
  rejected server-side (`FailedPrecondition`), but this UI doesn't yet
  present that as anything friendlier than the raw error message. See
  `admin-ui-implementation.md`'s "Multi-session concurrent editing" note.
- **Deliberate N+1.** Resolving a room/floor/building name for display (e.g.
  a device's "current location" badge, or a delete confirmation) is one RPC
  per level, not a server-side join - acceptable at admin-tool traffic
  levels (see `view.go`).
- **Flat room picker.** The room picker on `/devices` (link/move) lists every
  room as a flat `Building · Floor · Room` string (`allRoomOptions` in
  `view.go`), since `HouseService.ListRooms` has no unscoped "all rooms" RPC
  - it's built by walking `ListBuildings` → `ListFloors` → `ListRooms`. Known
  scaling gap once room counts grow - deferred per
  `admin-ui-implementation.md`.
- **Optional dependency, greyed out rather than broken.** `adminui.policy_addr`
  is the only optional dependency (house/bridge are mandatory - see main.go).
  Left unset, the Policies/Execution Log nav links are dimmed (`policyAvailable`
  in `templates.go`, backed by the `policyConfigured` package var `newServer`
  sets on every generation - see below) and every `/policies`/`/logs` route
  renders `policy_unavailable.html` with setup instructions instead of calling
  into the nil `PolicyServiceClient` (`requirePolicy` in `app.go`).
- **Live reconfiguration, not just config-file editing.** `/settings`
  (`settings.go`) edits `adminui.house_addr`/`bridge_facade_addr`/
  `policy_addr` and applies them without a restart: `app.rebuild` dials a
  brand new `*Server` generation (fresh gRPC conns, fresh device/policy
  hubs), persists the change into `adminui.yaml` via
  `configutil.PersistValues` (the same safe partial-write helper every
  bridge's `SetBridgeConfig` uses, not `viper.WriteConfig`), and only then
  atomically swaps it in as `app.current` - the old generation's hubs are
  cancelled and its conns closed once nothing new can be routed to them.
  Every other route indirects through `app.current` per request (`app.go`'s
  `handle`) rather than closing over one fixed `*Server`, which is what
  makes the swap possible without restarting the listener. `grpc.Dial` is
  lazy, so a save "succeeding" only means the new address was well-formed
  and persisted, not that anything is listening there yet - an unreachable
  address surfaces the same way it always has, as an ordinary RPC error on
  whatever page is opened next. A tab's `/events` connection is bound to
  whichever generation was current when it opened, so it goes quiet once
  that generation's hubs are torn down - `handleSettingsSave` responds to a
  successful save with `HX-Redirect` back to `/settings` rather than an
  in-place htmx swap, forcing the saving tab to reload and reopen `/events`
  against the new generation. Any *other* already-open tab doesn't get this
  for free and still needs a manual reload.
- **Live updates.** `sse.go` relays `BridgeService.StreamUpdates` as SSE,
  re-emitting each `DeviceUpdate` as an out-of-band swap of that device's
  `<td id="device-info-<id>">` cell (`templates/partials/device_info.html`) -
  wherever that id currently exists in the DOM, on any open page. Room-link
  action cells (unlink / link-move buttons) are separate elements, untouched
  by a device-state update. A newly *added* device doesn't appear live in an
  already-open list (no id to swap yet); a *removed* device's cell is
  replaced with a placeholder rather than the row being removed. Both are
  deliberate v1 simplifications - reload the page to see the current set.
  The same `/events` connection also relays `PolicyService.StreamEvents`
  (`policy_hub.go`) - one SSE connection per tab carries both house and
  policy live updates, rather than each opening its own.
