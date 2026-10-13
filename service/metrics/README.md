# metricsd

Records device state changes and commands from house into InfluxDB v2. It is
the only process that talks to Influx; `house` itself stays storage-free, and
nothing here needs cloud connectivity.

## Setup (do this first)

metricsd needs a bucket and a token in an existing InfluxDB v2 before it will
start. `scripts/setup-metrics-influx.sh` creates both, idempotently:

Easiest is from inside the influxdb container, whose CLI is already
authenticated if it was created with the image's `DOCKER_INFLUXDB_INIT_*`
variables (check with `influx config list`):

```sh
docker cp scripts/setup-metrics-influx.sh <container>:/tmp/
docker exec -e INFLUX_ORG=falnet -e SECRETS_OUT=/tmp/metricsd.secrets.yaml \
  <container> bash /tmp/setup-metrics-influx.sh
docker cp <container>:/tmp/metricsd.secrets.yaml ./metricsd.secrets.yaml
docker exec <container> rm -f /tmp/metricsd.secrets.yaml /tmp/setup-metrics-influx.sh
```

Or from any machine with the `influx` CLI that can reach the server:

```sh
INFLUX_HOST=http://<influx-host>:8086 \
INFLUX_TOKEN=<operator token> \
INFLUX_ORG=falnet \
scripts/setup-metrics-influx.sh
```

The operator token is only used by the script; metricsd never sees it.

It:

- creates bucket **`house`** - raw state and commands, **30 day retention**;
- creates bucket **`house_1h`** - hourly aggregates, **548 days (~1.5 years)**
  retention, for year-over-year comparison;
- creates a scheduled Flux task, **`metricsd-downsample`**, that rolls `house`
  up into `house_1h` every hour (see below);
- creates a token `metricsd` with **read and write on `house` only** (read is
  for the backfill watermark query), and proves the scoping by reading its
  own bucket and being refused on another one;
- writes that token to `./metricsd.secrets.yaml` (mode 600, refuses to
  overwrite without `FORCE=1`). Place it beside `metricsd.yaml`; the config
  refers to it as `token: !secret influx_token`.

Everything is overridable by environment variable (see the script header),
`DOWNSAMPLE=0` skips the aggregate bucket and task, and re-running is safe:
existing buckets and tokens are reused and the task is updated in place.

### How data ages

InfluxDB v2 has no compaction policy to configure (that was v1's
retention-policy model; compaction is automatic). What thins data out as it
ages is the downsample task:

| Age | Where | Resolution |
|---|---|---|
| 0 - 30 days | `house` | every point metricsd wrote (already deadband + heartbeat filtered) |
| 0 - ~1.5 years | `house_1h` | one point per hour per series |

Each hour the task re-aggregates the last 26h of `state` (hour-start
timestamps):

- numeric fields (floats, ints, enums) -> `agg=mean|min|max|last`, all as
  floats, so every aggregate of a field shares one type;
- bool fields -> `agg=last` only;
- `command` points are copied unchanged, so they outlive the raw bucket;
- tags are preserved, including `source`: live and backfilled points of one
  device stay separate series rather than being averaged together.

The 26h lookback is deliberate: backfilled history arrives with timestamps up
to 24h old, which a last-hour-only task would never aggregate. Re-aggregating
a window is idempotent.

Query `house` for recent detail and `house_1h` for anything older than 30
days. The `metricsd` token cannot read `house_1h`; point dashboards at their
own read token.

## Overview

`metricsd` connects to `housed` (the facade-embedding process) and consumes:

- `BridgeService.StreamUpdates` — device state, plus `CommandEvent`s for every
  command a bridge processed (see `api/bridge.proto`).
- `HouseService.ListDeviceLinks` — refreshed every `room_refresh_interval` to
  tag points with their room.

It issues no commands and writes nothing back to house.

## What is written

Bucket `house`, two measurements:

| Measurement | Tags | Fields |
|---|---|---|
| `state` | `bridge_id`, `device_id`, `device_name`, `room_id`, `trait`, `source` (`live`) | one field per numeric/bool/enum leaf of the trait's `State`, e.g. `co2_ppm`, `temperature_c`, `power_w` |
| `command` | `bridge_id`, `device_id`, `command_type`, `origin` | `outcome` (gRPC code), `detail` (JSON command body), `command_id`, `resulting_version` |

- One `state` point per trait per update, tagged `trait=air_quality`,
  `on_off`, `power`, … — the device message's field name, with no leading
  `light.`/`sensor.` segment (same as the policy `GetState` key convention).
- Strings, lists, timestamps and trait `Attributes` are not written; a
  sensor's `Metadata` is skipped as bookkeeping.
- Bools are written as booleans, enums as their integer value.
- `device_name` and `room_id` are resolved at write time; points written
  before a rename or room move keep the old values.
- `Command.origin` is advisory and unauthenticated; don't use it for access
  control.
- A `command` point exists only if the bridge that executed the command runs
  code that emits `CommandEvent`: each bridge publishes its own events, and
  housed only relays them. Until a bridge is rebuilt its commands are simply
  absent, while its state is recorded regardless. A command rejected before it
  reaches a bridge's handler (unknown device, malformed) is not recorded.

## History backfill

Devices that record their own samples (they advertise
`Sensor.Metadata.history` - currently the Airthings BLE sensors) are asked for
them on a schedule via `BridgeService.GetDeviceHistory`, so the bridge can
poll them rarely and the gaps are filled in afterwards. Results are written to
the same `state` measurement with `source=backfill`, bypassing the sampler.

- **Resume point:** the newest `source=backfill` timestamp for the device is
  read back from Influx (a Flux query), so a restarted or relocated metricsd
  continues where the data says it stopped. With none, it starts
  `max_window` ago. If Influx can't be read the device is skipped for that
  pass rather than refetched blind.
- **Schedule:** every `backfill.interval` (default 12h), first after
  `initial_delay`. Keep it well under the device's `max_window` (24h for
  Airthings) - the bridge can only be asked for "the last N hours", so a
  missed window is lost.
- **One device at a time**, since a bridge typically has one BLE adapter. A
  failure is logged and retried on the next pass; it never stops other
  devices.
- Skipped when less than one `sample_interval` has passed since the last
  backfilled sample, so no BLE session is spent for nothing.
- Re-fetching an overlapping window is harmless: Influx overwrites a point
  with identical measurement, tags and timestamp. (A device renamed or moved
  between rooms in between lands as a new series; the old points keep their
  old tags.)

Live and backfilled points of the same device are separate series (`source`
differs), so a query that wants one continuous series should select both.

## Exclusions

Network clients (what the omada bridge reports, as the `connected_device`
detail) are not recorded by default: there are many and they are noise for
now. `exclude.details` (device-kind names), `exclude.bridge_ids` and
`exclude.device_ids` control this; `details: []` records them again.
Exclusions apply to device state only; commands are always recorded.

## Sampling

Live numeric values are thinned with a deadband plus heartbeat (see
`sampling` in `cmd/metricsd/metricsd.example.yaml`): a field is written when
it is first seen since start, moves by at least its deadband from the last
*written* value, or has gone `heartbeat` (default 5m) unwritten — so a gap in
a series means "no data", not "no change". Bools and enums are written on any
change (and on the heartbeat). Commands are never filtered. `mode: none`
disables filtering.

After a reconnect, housed replays every device; those go through the same
sampler, so only values that actually differ are written again.

## If Influx is down

Writes are buffered (`influx.max_buffered`, default 100000 points). A failed
flush is retried on the next interval; once the buffer is full the oldest
points are dropped and counted, rather than back-pressuring the update stream.
A batch Influx rejects outright (HTTP 4xx other than 408/429) is dropped, since
retrying it would wedge the queue.

## Running

```sh
bazel run //service/metrics/cmd/metricsd
```

Config is `metricsd.yaml` in `/etc/house`, `$HOME/.config/house` or `.`; see
`cmd/metricsd/metricsd.example.yaml`. The Influx token is read from a sibling
`metricsd.secrets.yaml` via a `!secret influx_token` reference.

The bucket and token must exist before metricsd starts (see Setup above); it
does not create buckets.
