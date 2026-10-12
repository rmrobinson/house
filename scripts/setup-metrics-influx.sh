#!/usr/bin/env bash
# One-time InfluxDB v2 setup for metricsd (service/metrics). Creates:
#
#   house      raw device state + commands, 30 day retention
#   house_1h   hourly aggregates (mean/min/max/last), ~1.5 year retention
#
# a token that can read and write `house` and nothing else (what metricsd
# uses), and a scheduled Flux task that rolls `house` up into `house_1h`.
# Safe to re-run: existing buckets, tokens and the task are reused or updated,
# never duplicated.
#
# Run it with an operator / all-access token from any machine that has the
# `influx` CLI and can reach the server - or inside the influxdb container:
#
#   INFLUX_HOST=http://<influx-host>:8086 \
#   INFLUX_TOKEN=<operator token> \
#   scripts/setup-metrics-influx.sh
#
#   # or, from the Docker host:
#   docker cp scripts/setup-metrics-influx.sh <container>:/tmp/
#   docker exec -e INFLUX_TOKEN=<operator token> <container> bash /tmp/setup-metrics-influx.sh
#
# The CLI reads INFLUX_HOST / INFLUX_TOKEN / INFLUX_ORG itself.
#
# Environment:
#   INFLUX_HOST          server URL (required, unless running on the server itself)
#   INFLUX_TOKEN         operator / all-access token (required; used only here)
#   INFLUX_ORG           organization (default: falnet)
#   METRICS_BUCKET       raw bucket (default: house)
#   METRICS_RETENTION    raw retention (default: 720h = 30 days)
#   DOWNSAMPLE=0         skip the aggregate bucket and task
#   DOWNSAMPLE_BUCKET    aggregate bucket (default: house_1h)
#   DOWNSAMPLE_RETENTION aggregate retention (default: 13152h = 548 days)
#   DOWNSAMPLE_LOOKBACK  how far back each hourly run re-aggregates (default: 26h)
#   SECRETS_OUT          where to write metricsd's token (default: ./metricsd.secrets.yaml)
#   FORCE=1              overwrite SECRETS_OUT if it already exists
#
# About "compaction": InfluxDB v2 has no user-configurable compaction policy
# (that was a v1 retention-policy concept); compaction is automatic. What
# thins data out as it ages is the downsample task below: raw points are kept
# 30 days, hourly aggregates for ~1.5 years. Retention and shard group
# duration are the only storage knobs, and are set per bucket.
#
# The lookback is deliberately longer than a day: backfilled device history
# (service/metrics Phase 4) arrives with timestamps up to 24h old, so a task
# that only looked at the last hour would never aggregate it. Re-aggregating
# the same window is idempotent - Influx overwrites identical series+time.
set -euo pipefail

ORG="${INFLUX_ORG:-falnet}"
RAW="${METRICS_BUCKET:-house}"
RAW_RETENTION="${METRICS_RETENTION:-720h}"
DOWNSAMPLE="${DOWNSAMPLE:-1}"
AGG="${DOWNSAMPLE_BUCKET:-house_1h}"
AGG_RETENTION="${DOWNSAMPLE_RETENTION:-13152h}"
LOOKBACK="${DOWNSAMPLE_LOOKBACK:-26h}"
SECRETS_OUT="${SECRETS_OUT:-./metricsd.secrets.yaml}"

METRICSD_TOKEN_DESC="metricsd"
DOWNSAMPLE_TOKEN_DESC="metricsd-downsample"
TASK_NAME="metricsd-downsample"

die() { echo "error: $*" >&2; exit 1; }

command -v influx >/dev/null 2>&1 || die "the influx CLI is not installed; run this inside the influxdb container instead (see the header of this script)"
[ -n "${INFLUX_TOKEN:-}" ] || die "INFLUX_TOKEN (an operator/all-access token) is required"

if [ -e "$SECRETS_OUT" ] && [ "${FORCE:-}" != "1" ]; then
  die "$SECRETS_OUT already exists; set FORCE=1 to overwrite or SECRETS_OUT to another path"
fi

echo "==> checking connection"
influx ping >/dev/null || die "cannot reach InfluxDB (INFLUX_HOST=${INFLUX_HOST:-<unset>})"
influx org list --name "$ORG" --hide-headers >/dev/null || die "org '$ORG' not found or not visible to this token"

# --- buckets --------------------------------------------------------------
bucket_id() {
  influx bucket list --org "$ORG" --name "$1" --hide-headers 2>/dev/null | awk 'NR==1 {print $1}'
}

# ensure_bucket <name> <retention> <shard-group-duration> <description>
ensure_bucket() {
  local id
  id="$(bucket_id "$1" || true)"
  if [ -z "$id" ]; then
    echo "==> creating bucket '$1' (retention $2, shard group $3)" >&2
    influx bucket create --org "$ORG" --name "$1" --retention "$2" \
      --shard-group-duration "$3" --description "$4" >/dev/null
    id="$(bucket_id "$1")"
  else
    echo "==> bucket '$1' already exists ($id); leaving its settings alone" >&2
  fi
  [ -n "$id" ] || die "could not determine the ID of bucket '$1'"
  echo "$id"
}

RAW_ID="$(ensure_bucket "$RAW" "$RAW_RETENTION" 24h "metricsd: raw device state and commands")"
AGG_ID=""
if [ "$DOWNSAMPLE" = "1" ]; then
  AGG_ID="$(ensure_bucket "$AGG" "$AGG_RETENTION" 168h "metricsd: hourly aggregates of $RAW")"
fi

# --- tokens ---------------------------------------------------------------
# existing_token <description>: prints the token, or nothing.
existing_token() {
  influx auth list --org "$ORG" --json 2>/dev/null \
    | tr -d '\n' \
    | grep -o '{[^{}]*"description": *"'"$1"'"[^{}]*}' \
    | grep -o '"token": *"[^"]*"' | head -1 | sed 's/.*: *"\(.*\)"/\1/' || true
}

# create_token <description> <influx auth create permission flags...>
ensure_token() {
  local desc="$1"; shift
  local tok
  tok="$(existing_token "$desc")"
  if [ -z "$tok" ]; then
    echo "==> creating token '$desc'" >&2
    tok="$(influx auth create --org "$ORG" --description "$desc" "$@" --json \
      | tr -d '\n' | grep -o '"token": *"[^"]*"' | head -1 | sed 's/.*: *"\(.*\)"/\1/')"
  else
    echo "==> token '$desc' already exists; reusing it" >&2
  fi
  [ -n "$tok" ] || die "could not obtain token '$desc'"
  echo "$tok"
}

# metricsd: read (backfill watermark query) and write on the raw bucket only.
METRICSD_TOKEN="$(ensure_token "$METRICSD_TOKEN_DESC" --read-bucket "$RAW_ID" --write-bucket "$RAW_ID")"

# --- downsample task ------------------------------------------------------
if [ "$DOWNSAMPLE" = "1" ]; then
  # A task runs with the permissions of the token that creates it, so it is
  # created with its own token - read raw, write aggregates - rather than
  # leaving the operator token embedded in it. Creating a task also requires
  # task permissions on that token. The token is used only here and by the
  # server; it is never written out or handed to metricsd.
  TASK_TOKEN="$(ensure_token "$DOWNSAMPLE_TOKEN_DESC" \
    --read-bucket "$RAW_ID" --write-bucket "$AGG_ID" --read-tasks --write-tasks)"

  TASK_FILE="$(mktemp)"
  trap 'rm -f "$TASK_FILE"' EXIT
  cat > "$TASK_FILE" <<FLUX
import "types"

option task = {name: "$TASK_NAME", every: 1h, offset: 5m}

raw = from(bucket: "$RAW")
    |> range(start: -$LOOKBACK)

state = raw |> filter(fn: (r) => r._measurement == "state")

// Numeric fields become float for all four aggregates: a field key holds one
// type per measurement per shard, so mean (float) and min/max/last (would be
// int) of the same field must not differ. Enums are numeric here too.
numeric = state
    |> filter(fn: (r) => types.isType(v: r._value, type: "float") or types.isType(v: r._value, type: "int"))
    |> toFloat()

numeric
    |> aggregateWindow(every: 1h, fn: mean, createEmpty: false, timeSrc: "_start")
    |> set(key: "agg", value: "mean")
    |> to(bucket: "$AGG", org: "$ORG")

numeric
    |> aggregateWindow(every: 1h, fn: min, createEmpty: false, timeSrc: "_start")
    |> set(key: "agg", value: "min")
    |> to(bucket: "$AGG", org: "$ORG")

numeric
    |> aggregateWindow(every: 1h, fn: max, createEmpty: false, timeSrc: "_start")
    |> set(key: "agg", value: "max")
    |> to(bucket: "$AGG", org: "$ORG")

numeric
    |> aggregateWindow(every: 1h, fn: last, createEmpty: false, timeSrc: "_start")
    |> set(key: "agg", value: "last")
    |> to(bucket: "$AGG", org: "$ORG")

// Booleans have no mean; their last value in the hour is what's kept.
state
    |> filter(fn: (r) => types.isType(v: r._value, type: "bool"))
    |> aggregateWindow(every: 1h, fn: last, createEmpty: false, timeSrc: "_start")
    |> set(key: "agg", value: "last")
    |> to(bucket: "$AGG", org: "$ORG")

// Commands are sparse discrete events: kept as they are, so they survive the
// raw bucket's retention.
raw
    |> filter(fn: (r) => r._measurement == "command")
    |> to(bucket: "$AGG", org: "$ORG")
FLUX

  TASK_ID="$(INFLUX_TOKEN="$TASK_TOKEN" influx task list --org "$ORG" --hide-headers 2>/dev/null \
    | awk -v n="$TASK_NAME" '$2 == n {print $1; exit}')"
  if [ -z "$TASK_ID" ]; then
    echo "==> creating task '$TASK_NAME' ($RAW -> $AGG, hourly, lookback $LOOKBACK)"
    INFLUX_TOKEN="$TASK_TOKEN" influx task create --org "$ORG" --file "$TASK_FILE" >/dev/null
  else
    echo "==> updating existing task '$TASK_NAME' ($TASK_ID)"
    INFLUX_TOKEN="$TASK_TOKEN" influx task update --id "$TASK_ID" --file "$TASK_FILE" >/dev/null
  fi
fi

# --- verify ---------------------------------------------------------------
# Prove the metricsd token's scoping with the token itself: it can read its
# bucket and is refused on every other one. (Read-only probes; nothing is
# written.)
echo "==> verifying the metricsd token"
if ! INFLUX_TOKEN="$METRICSD_TOKEN" influx query --org "$ORG" \
    "from(bucket: \"$RAW\") |> range(start: -1m) |> limit(n: 1)" >/dev/null 2>&1; then
  die "the metricsd token cannot read bucket '$RAW'"
fi

OTHER="$(influx bucket list --org "$ORG" --hide-headers 2>/dev/null | awk -v b="$RAW" '$2 != b && $2 !~ /^_/ {print $2; exit}')"
if [ -n "$OTHER" ]; then
  if INFLUX_TOKEN="$METRICSD_TOKEN" influx query --org "$ORG" \
      "from(bucket: \"$OTHER\") |> range(start: -1m) |> limit(n: 1)" >/dev/null 2>&1; then
    die "the metricsd token can read bucket '$OTHER' - it is not scoped to '$RAW' alone"
  fi
  echo "    ok: read on '$RAW' allowed, read on '$OTHER' refused"
else
  echo "    ok: read on '$RAW' allowed (no other bucket to test refusal against)"
fi

# --- output ---------------------------------------------------------------
umask 077
printf 'influx_token: %s\n' "$METRICSD_TOKEN" > "$SECRETS_OUT"
chmod 600 "$SECRETS_OUT"

cat <<EOF

Done.
  raw bucket:        $RAW ($RAW_ID), retention $RAW_RETENTION
EOF
if [ "$DOWNSAMPLE" = "1" ]; then
  cat <<EOF
  aggregate bucket:  $AGG ($AGG_ID), retention $AGG_RETENTION
  downsample task:   $TASK_NAME, hourly, re-aggregating the last $LOOKBACK
EOF
fi
cat <<EOF
  metricsd token:    written to $SECRETS_OUT (mode 600) - keep it out of git

In metricsd.yaml:

  metricsd:
    influx:
      url: ${INFLUX_HOST:-http://<influx-host>:8086}
      org: $ORG
      bucket: $RAW
      token: !secret influx_token

Next: put $SECRETS_OUT beside metricsd.yaml as metricsd.secrets.yaml.
EOF
