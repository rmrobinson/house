# adminui

Server-rendered admin UI for house/floor/room topology and device-to-room
linking. `html/template` + [htmx](https://htmx.org) for partial re-renders -
no JS SPA framework, no grpc-web. htmx and its
[SSE extension](https://htmx.org/extensions/sse/) are vendored under
`static/`, not pulled from a CDN, so the app has no runtime dependency on
outside network access.

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
to a separately-run `bridgefacaded` instead.

## Pages

| Route | Purpose |
|---|---|
| `/buildings` | List + create buildings |
| `/buildings/{id}` | List + create floors in a building; delete building |
| `/floors/{id}` | Edit name/sort_order; list + create rooms; delete floor |
| `/rooms/{id}` | Linked devices, unlink, add-device picker; delete room |
| `/devices` | All devices, All/Unlinked toggle, link/move picker |

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
- **Live updates.** `sse.go` relays `BridgeService.StreamUpdates` as SSE,
  re-emitting each `DeviceUpdate` as an out-of-band swap of that device's
  `<td id="device-info-<id>">` cell (`templates/partials/device_info.html`) -
  wherever that id currently exists in the DOM, on any open page. Room-link
  action cells (unlink / link-move buttons) are separate elements, untouched
  by a device-state update. A newly *added* device doesn't appear live in an
  already-open list (no id to swap yet); a *removed* device's cell is
  replaced with a placeholder rather than the row being removed. Both are
  deliberate v1 simplifications - reload the page to see the current set.
