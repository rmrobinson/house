# Grafana dashboard for metricsd

`house-metrics.json` is a Grafana dashboard over the two buckets metricsd
maintains: `house` (raw state + commands, 30 days) and `house_1h` (hourly
aggregates, ~1.5 years). Nothing here runs in this repo; Grafana is deployed
separately (net001, next to InfluxDB).

## 1. A read-only token

`scripts/setup-metrics-influx.sh` creates a token described `metricsd-grafana` (not `grafana`, which an existing
Grafana datasource's token may already use) with
**read on `house` and `house_1h` and nothing else**, and checks that a write
with it is refused. It is written to `GRAFANA_TOKEN_OUT` (default
`./grafana.token`, mode 600). The script is idempotent, so re-running it on an
existing install only adds this token; since it refuses to overwrite
`metricsd.secrets.yaml`, point `SECRETS_OUT` at a scratch path (or set
`FORCE=1`) when you re-run it:

```sh
docker cp scripts/setup-metrics-influx.sh <influx-container>:/tmp/
docker exec -e INFLUX_ORG=falnet \
  -e SECRETS_OUT=/tmp/metricsd.secrets.yaml.rerun \
  -e GRAFANA_TOKEN_OUT=/tmp/grafana.token \
  <influx-container> bash /tmp/setup-metrics-influx.sh
docker cp <influx-container>:/tmp/grafana.token ./grafana.token
docker exec <influx-container> rm -f /tmp/setup-metrics-influx.sh /tmp/grafana.token /tmp/metricsd.secrets.yaml.rerun
```

The existing `metricsd` and `metricsd-downsample` tokens and the task are
reused, not recreated.

## 2. The datasource

Grafana -> Connections -> Data sources -> Add -> **InfluxDB**:

| Setting | Value |
|---|---|
| Query language | **Flux** |
| URL | `http://10.17.20.150:8086` (influxdb on net1720) |
| Organization | `falnet` |
| Token | contents of `grafana.token` |
| Default bucket | `house` |

Save & test should report "datasource is working".

## 3. Import

Dashboards -> New -> Import -> upload `house-metrics.json`, pick the datasource
from step 2 when prompted. (The file uses an `__inputs` placeholder, so it
isn't tied to a datasource UID.)

## How the panels choose a bucket

Every query starts with:

```flux
bucketName = if v.timeRangeStart < experimental.subDuration(d: 28d, from: now())
  then "house_1h" else "house"
```

A range that starts within the last 28 days reads raw points from `house`;
anything reaching further back reads `house_1h` (raw is only kept 30 days, so
the cut-over leaves margin). On `house_1h` the panels select `agg == "mean"`
and the window is never shorter than one hour.

Things to know when reading the graphs:

- **Series are per device name**, merged across the `live` and `backfill`
  sources (otherwise a device with backfilled history would draw twice).
- **Numeric fields only.** Bool fields (e.g. `on_off`, `water_detected`) have
  only an `agg=last` rollup and aren't charted; add a panel filtering
  `r.agg == "last"` if you want one.
- **Hourly points are labelled at the end of their hour** by `aggregateWindow`.
- **Devices without a name** have no `device_name` tag and don't match the
  `device` filter; fix by naming them (`Config.Name`, see the bridge docs).
- Illuminance isn't charted: Airthings reports a percentage, not lux, so it
  isn't mapped (see the metricsd README).
- Telegraf's older `network-metrics` data is untouched and not in this
  dashboard.

## Adding a panel

Copy a panel and change `r.trait` / `r._field` in its query. A trait is the
`Device` message's field name (`air_properties`, `air_quality`, `power`,
`battery`, ...) and the field is the `State` leaf (`temperature_c`,
`co2_ppm`, `power_w`, ...), exactly as listed in the metricsd README's
"What is written".
