# facade

`facade` implements `BridgeService` itself, backed by upstream connections to
a configured set of individual bridges (ESPHome, Z-Wave, webOS, Cast,
HomeKit-controller, etc). A client holds one connection to the facade instead
of dialing each bridge directly.

- **Its own identity.** The facade is itself a `Bridge`, with its own minted
  `Bridge.Id` (same generate-and-persist pattern as an individual bridge -
  see `cmd/bridgefacaded/main.go`) and its own advertised network address
  (`bridge.address` in config). It's seeded into the cache at construction,
  so `GetBridge` on its own ID works like any other bridge.
- **Upstream connections.** One `BridgeService` client connection per
  configured bridge address, config-driven (a list of addresses, same
  YAML/viper pattern as the ESPHome bridge - see `cmd/bridgefacaded`).
- **Fan-in.** `Connect` calls `StreamUpdates({})` on each upstream bridge and
  keeps the connection open, reconnecting with backoff on drop.
- **Local cache, not passthrough queries.** `Facade` maintains
  `device_id -> (owning bridge_id, last-known Device)` and
  `bridge_id -> (upstream connection, last-known Bridge)`, built entirely
  from observed `Update` messages, exactly as reported by the owning
  upstream bridge. `GetBridge`/`ListDevices`/`GetDevice` are served from this
  cache, never round-tripped upstream.
- **One more hop in the mesh.** `Device.Address` already models bridges
  proxying devices across a mesh (`address`, `is_reachable`, `hop_count`).
  The facade participates in that model as one more hop: every `Device` it
  hands to a downstream client - via `GetDevice`/`ListDevices`/
  `StreamUpdates`, or as the return value of a forwarded write - has
  `Address` rewritten to the facade's own advertised address with
  `hop_count` incremented by one (see `present()` in `facade.go`), so a
  client only ever needs to know how to reach the facade, never an
  individual upstream bridge. The cache itself keeps the untouched
  address/hop_count exactly as the owning bridge reported it.
- **Downstream re-publish.** Every non-initial `Update` received from any
  upstream connection is re-emitted to the facade's own `StreamUpdates`
  subscribers via `service/bridge.Source`/`Sink`. `BridgeUpdate` and
  `CommandUpdate` are forwarded unchanged - each already self-identifies its
  `bridge_id`/`device_id` and carries no per-hop address. A `DeviceUpdate`'s
  `Device` gets the same `Address` rewrite described above before
  publishing.
- **Inbound call routing.** `UpdateDeviceConfig` and `ExecuteCommand(Async)`
  look up the owning bridge for the request's device ID in the cache -
  `bridge_id`/ownership is untouched by the address rewrite above, it's a
  separate concern - and forward the request unchanged; the `Device` in the
  response is presented the same way as any other, and any resulting
  cache/update-stream change is observed asynchronously off that bridge's
  update stream like any other change.
- **Synthesized `INITIAL`.** A raw upstream `InitialUpdate` is never
  forwarded downstream - a client subscribing to the facade has no
  visibility into when each upstream bridge last reconnected, so replaying
  one verbatim would be ambiguous. Instead, on every new subscription the
  facade synthesizes its own initial burst from the cache: one `BridgeUpdate`
  (`INITIAL`) per known bridge, then one `DeviceUpdate` (`INITIAL`) per known
  device.
- **Failure handling.** When an upstream bridge disconnects, the facade marks
  that bridge and its devices unreachable (`Bridge.is_reachable` /
  `Device.Address.is_reachable`) and emits synthetic `BridgeUpdate`/
  `DeviceUpdate`s reflecting the change, rather than dropping them from
  state. On reconnect, the facade re-syncs that bridge's device set from its
  next `InitialUpdate` and emits the corresponding `ADDED`/`CHANGED`/
  `REMOVED` updates.

Individual bridges are untouched by this package - each keeps running as a
standalone `BridgeService` server, unaware it's being proxied.
