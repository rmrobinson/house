# googlehome

Integrates `house` with the Google Smart Home Action framework ("Hey Google,
turn on the kitchen lights"), structurally modeled on the
[`nerves`](https://github.com/rmrobinson/nerves) `services/domotics/
integrations/googlehome` precedent and built on
[`google-smart-home-action-go`](https://github.com/rmrobinson/google-smart-home-action-go).

Two daemons, split the same way the nerves precedent splits them - so the
fulfillment webhook never needs a direct network path into the home
network, only the reverse:

- **`cmd/googlehomed`** is public-facing: it hosts the fulfillment webhook
  Google calls (SYNC/QUERY/EXECUTE/DISCONNECT), talks to HomeGraph, and
  exposes `RelayService` for the agent to dial into.
- **`cmd/googlehome-agentd`** runs on the home network: it dials out to
  `googlehomed`'s `RelayService`, and translates between it and a
  `BridgeService` endpoint (`housed`, `bridgefacaded`, or a single bridge -
  see `agent/`).

## Packages

- **`relay`** implements the agent<->relay wire protocol
  (`googlehome.proto`) and the server side of it: `API` is the gRPC
  `RelayService` server, `ProviderService` satisfies
  `action.Provider` by looking up whichever agent is currently registered
  (house is single-building, so in practice there is exactly one), and
  `RemoteProvider` is the per-agent `action.Provider` that relays
  SYNC/QUERY/EXECUTE over that agent's stream.
- **`agent`** implements the translation: `deviceToAction`/`stateToAction`/
  `buildCommand` (in `translate.go`) convert between `api/device`+
  `api/command` and the `action` library's `Device`/`DeviceState`/`Command`
  types, and `Agent` (in `agent.go`) wires a reconnecting `BridgeService`
  connection together with one `RelayService` stream.

## Device support

Only `api/device/light.proto`'s `Light` is translated today (on/off,
brightness, colour in either RGB or HSB mode, colour temperature) - that's
the device kind people actually ask Google Assistant to control. Extending
to another device kind means adding a case to `deviceToAction`/
`stateToAction`/`buildCommand` in `agent/translate.go` and nowhere else.

## What you still need to do yourself

This integration needs a few things only you can set up, none of which
belong in this repo (see `AGENTS.md`'s "No deployment-specific addressing"):

- Register a Smart Home Action project in the Google Actions console,
  pointing its fulfillment URL at wherever `googlehomed` ends up publicly
  reachable (`https://<your-public-host>/fulfillment`), and configure its
  OAuth2 account-linking authorization/token endpoints to always issue the
  same shared-secret token `googlehomed`'s config holds - see
  `cmd/googlehomed/auth.go`'s `staticTokenValidator` doc comment for why a
  single-user deployment doesn't need a real OAuth2 server.
- Create a Google service account with HomeGraph API access under that same
  project, and point `googlehome.homegraph.creds_file` at its key.
- Put `googlehomed` behind a reverse proxy (Caddy, with ACME) for the
  publicly-trusted TLS certificate Google requires on the fulfillment
  webhook - house-config already does this for its other public-facing
  services.
- All real addressing (where `googlehomed` runs, `googlehome-agentd`'s
  `bridge_addr`/`relay_addr`, TLS cert paths) lives in house-config, not
  here - see each `cmd/*/`.*.example.yaml`.
