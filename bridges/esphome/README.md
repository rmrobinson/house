# esphome bridge

Bridges ESP32 nodes running [ESPHome](https://esphome.io) into the `house` ecosystem over
ESPHome's native API (Noise-encrypted or plaintext).

## Nodes vs. devices

A single ESPHome node commonly hosts multiple unrelated entities (a light, a couple of switches,
a sensor, all sharing one native API connection). Config is therefore structured per `house`
device, not per node: each node lists one or more devices, and each device claims a subset of
that node's entities by `object_id` via a `roles` map. See `esphome.example.yaml`.

Only manual (config-driven) entity matching is supported today — newer ESPHome firmware can
report native entity groupings (`DeviceInfoResponse.devices`), but nothing in this bridge uses
that yet.

## Supported device types

- `clock` — on/off + brightness from a single ESPHome light entity, plus a bridge-tracked Time
  trait. ESPHome nodes don't have a "set time" API message; instead they periodically ask the
  bridge what time it is (`GetTimeRequest`), which this bridge answers using the current UTC
  time and the clock device's configured timezone.
- `light` — on/off + brightness from a single ESPHome light entity.
- `fan` — on/off + speed from a native ESPHome `fan` entity, a mode select, a fixed-option timer
  select (rejects any commanded duration outside its 13 fixed values rather than rounding to the
  nearest one), an optional temperature sensor, and any of `oscillation`/`beep`/`display` (each a
  separate switch entity, exposed together as one `Toggle` trait keyed by role name).
- `standing_desk` — read-only height (`Position`, `supports_set: false`) plus preset buttons
  (`Mode`, one role per preset). `Movement` (drive-by-switch Move Up/Down) is in the schema but
  intentionally not wired up by this builder yet — the desk it was built against has a known
  crash bug when driven via movement commands; that needs to be resolved before it's safe to
  exercise end-to-end. `position_min`/`position_max` (the desk's physical travel range, in the
  same unit as its height sensor) aren't reported by ESPHome and must be set in config.

Additional device types can be added by implementing the `deviceBuilder` interface in
`devices.go` and registering it in `deviceBuilders`.

## Config

See `esphome.example.yaml`. Config is loaded once at startup; there's no hot-reload yet (tracked
in the repo's `TODO.md`).

## Setting up against a real device

This walks through pointing the bridge at an actual ESP32/ESP8266 node, including turning on and
verifying Noise encryption. `bridges/esphome/*_test.go` covers the plaintext path against an
in-process fake server; this is the closest thing to an e2e guide for the real wire protocol,
including the part that can't be tested without real crypto (see `TODO.md`).

### 1. Prerequisites

- The [`esphome`](https://esphome.io) CLI installed (`brew install esphome`, or see their docs) —
  only needed to flash/configure the node, not to run this bridge.
- A node already flashed with ESPHome, or one you're about to flash, reachable from wherever this
  bridge runs (same L3 subnet, or routed — the bridge doesn't do mDNS/VLAN discovery, see the
  original design notes; you give it an address directly).

### 2. Enable the native API + Noise encryption on the node

In the node's own ESPHome YAML:

```yaml
api:
  encryption:
    key: "<base64 32-byte key>"
```

Generate the key with `openssl rand -base64 32` (or ESPHome's own dashboard "Add API Encryption"
wizard, which does the same thing). Flash/update the node with `esphome run <node>.yaml`.

Plaintext (no `encryption:` block) also works — the bridge supports both — but real deployments
should use Noise; ESPHome's own docs treat the API as effectively unauthenticated without it.

### 3. Find the node's object_ids

The bridge matches entities by `object_id`, not by their display name. Get the authoritative list
straight from the node rather than guessing at the slugified form of a name:

```sh
go run github.com/richard87/esphome-apiclient/cmd/esphome-cli entities \
  --address <node-ip>:6053 --key "<base64 psk>"
```

(Drop `--key` for a plaintext node.) This prints every entity with its `object_id` and key — use
the `object_id` values in the `roles` map below. The MAC address for the `mac:` config field is
cosmetic (used for logging only, not for connecting) — grab it from the node's boot log or its
`esphome logs` output if you want it recorded.

### 4. Configure the bridge

Copy `esphome.example.yaml`, fill in the node's address, PSK, and the object_ids from step 3:

```yaml
bridge:
  id: "<bridge ID>"
  listen_port: 17010

esphome:
  nodes:
    - mac: "AA:BB:CC:DD:EE:FF"
      address: "<node-ip>:6053"
      noise_psk: "<base64 psk>"       # omit entirely for a plaintext node
      devices:
        - id: office-lamp
          type: light
          roles:
            light: <object_id from step 3>
```

### 5. Run the bridge and verify

```sh
bazel build //bridges/esphome
bazel-bin/bridges/esphome/esphome_/esphome   # run from the directory containing your config
```

Then, using `bridgecli` (`bazel build //clients/bridgecli`):

```sh
bridgecli --addr 127.0.0.1:17010 bridge --bridgeID <bridge ID> listDevices
bridgecli --addr 127.0.0.1:17010 device --deviceID office-lamp onoff --on
```

Confirm the command actually reached the node (not just that the bridge's own optimistic cache
updated) by watching the node's physical state or its own logs (`esphome logs <node>.yaml`).

### 6. Verify Noise is actually being enforced

A bridge that silently fell back to plaintext, or wasn't checking the PSK at all, would look
identical to a working one in the happy path — the only way to know Noise is genuinely in effect
is to prove the bridge *rejects* the wrong key. With `noise_psk` configured, temporarily change it
to a different (also valid-looking, still 32-byte-base64) value and restart the bridge. It should
fail to connect with something like:

```
unable to connect to node, retrying  error: "noise handshake failed: invalid encryption key: Handshake MAC failure"
```

If you instead see a normal connection, the bridge isn't actually enforcing the key — stop and
investigate before trusting it in production. Put the correct PSK back once confirmed.

This exact procedure (correct-PSK-connects, wrong-PSK-rejected) was used to verify Noise support
against ESPHome's own "host" platform simulator before this guide was written, so the steps and
the specific error text above are confirmed accurate, not speculative — see the ESPHome docs on
the [`host` platform](https://esphome.io/components/host) if you want to build the same kind of
disposable local test node instead of using real hardware for this step.

### Troubleshooting

- `connection refused` — wrong address/port, or the node isn't up yet.
- `noise handshake failed: invalid encryption key: Handshake MAC failure` — PSK mismatch between
  the node's `api.encryption.key` and the bridge's `noise_psk`.
- `configured entity not found on node` (bridge log, at connect/reconnect) — the `object_id` in
  `roles` doesn't match what the node reports; re-run the `esphome-cli entities` command from step
  3 against the live node to confirm the current value.
