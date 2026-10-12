# metricsd

Records device state changes and commands from house into InfluxDB v2. It is
the only process that talks to Influx; `house` itself stays storage-free, and
nothing here needs cloud connectivity.

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

The bucket (and its 30-day retention) must exist before metricsd starts; it
does not create buckets.
