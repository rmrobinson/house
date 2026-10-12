#!/usr/bin/env bash
# One-time InfluxDB v2 setup for metricsd (service/metrics): creates the raw
# metrics bucket with its retention, and a token that can read and write that
# bucket and nothing else. Safe to re-run: an existing bucket or token is
# reused, never duplicated.
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
#   INFLUX_HOST        server URL (required, unless running on the server itself)
#   INFLUX_TOKEN       operator / all-access token (required; used only here)
#   INFLUX_ORG         organization (default: falnet)
#   METRICS_BUCKET     bucket name (default: house)
#   METRICS_RETENTION  raw retention (default: 720h, i.e. 30 days)
#   METRICS_SHARD_DURATION  shard group duration (default: 24h)
#   METRICS_TOKEN_DESC token description (default: metricsd)
#   SECRETS_OUT        where to write the new token (default: ./metricsd.secrets.yaml)
#   FORCE=1            overwrite SECRETS_OUT if it already exists
#
# About "compaction": InfluxDB v2 has no user-configurable compaction policy
# (that was a v1 retention-policy concept). Compaction is automatic. The two
# knobs that exist are retention (how long points are kept) and shard group
# duration (the time span each shard covers, which sets the unit of expiry
# and compaction). 24h shards suit 30 days of retention.
set -euo pipefail

ORG="${INFLUX_ORG:-falnet}"
BUCKET="${METRICS_BUCKET:-house}"
RETENTION="${METRICS_RETENTION:-720h}"
SHARD="${METRICS_SHARD_DURATION:-24h}"
DESC="${METRICS_TOKEN_DESC:-metricsd}"
SECRETS_OUT="${SECRETS_OUT:-./metricsd.secrets.yaml}"

die() { echo "error: $*" >&2; exit 1; }

command -v influx >/dev/null 2>&1 || die "the influx CLI is not installed; run this inside the influxdb container instead (see the header of this script)"
[ -n "${INFLUX_TOKEN:-}" ] || die "INFLUX_TOKEN (an operator/all-access token) is required"

if [ -e "$SECRETS_OUT" ] && [ "${FORCE:-}" != "1" ]; then
  die "$SECRETS_OUT already exists; set FORCE=1 to overwrite or SECRETS_OUT to another path"
fi

echo "==> checking connection"
influx ping >/dev/null || die "cannot reach InfluxDB (INFLUX_HOST=${INFLUX_HOST:-<unset>})"
influx org list --name "$ORG" --hide-headers >/dev/null || die "org '$ORG' not found or not visible to this token"

# --- bucket ---------------------------------------------------------------
bucket_id() {
  influx bucket list --org "$ORG" --name "$BUCKET" --hide-headers 2>/dev/null | awk 'NR==1 {print $1}'
}

BUCKET_ID="$(bucket_id || true)"
if [ -z "$BUCKET_ID" ]; then
  echo "==> creating bucket '$BUCKET' (retention $RETENTION, shard group $SHARD)"
  influx bucket create \
    --org "$ORG" \
    --name "$BUCKET" \
    --description "metricsd: raw device state and commands" \
    --retention "$RETENTION" \
    --shard-group-duration "$SHARD" >/dev/null
  BUCKET_ID="$(bucket_id)"
else
  echo "==> bucket '$BUCKET' already exists ($BUCKET_ID); leaving its settings alone"
fi
[ -n "$BUCKET_ID" ] || die "could not determine the bucket ID"

# --- token ----------------------------------------------------------------
# Scoped to exactly this bucket: read (backfill watermark query) and write.
# It can't create buckets, read other buckets, or manage tasks or users.
existing_token() {
  influx auth list --org "$ORG" --json 2>/dev/null \
    | tr -d '\n' \
    | grep -o '{[^{}]*"description": *"'"$DESC"'"[^{}]*}' \
    | grep -o '"token": *"[^"]*"' | head -1 | sed 's/.*: *"\(.*\)"/\1/' || true
}

TOKEN="$(existing_token)"
if [ -z "$TOKEN" ]; then
  echo "==> creating token '$DESC' (read+write on bucket '$BUCKET' only)"
  TOKEN="$(influx auth create \
    --org "$ORG" \
    --description "$DESC" \
    --read-bucket "$BUCKET_ID" \
    --write-bucket "$BUCKET_ID" \
    --json | tr -d '\n' | grep -o '"token": *"[^"]*"' | head -1 | sed 's/.*: *"\(.*\)"/\1/')"
else
  echo "==> token '$DESC' already exists; reusing it"
fi
[ -n "$TOKEN" ] || die "could not obtain the token"

# --- verify ---------------------------------------------------------------
# Prove the scoping with the new token itself: it can read its bucket and is
# refused on every other one. (A read-only probe, so nothing is written.)
echo "==> verifying the token"
if ! INFLUX_TOKEN="$TOKEN" influx query --org "$ORG" \
    "from(bucket: \"$BUCKET\") |> range(start: -1m) |> limit(n: 1)" >/dev/null 2>&1; then
  die "the new token cannot read bucket '$BUCKET'"
fi

OTHER="$(influx bucket list --org "$ORG" --hide-headers 2>/dev/null | awk -v b="$BUCKET" '$2 != b && $2 !~ /^_/ {print $2; exit}')"
if [ -n "$OTHER" ]; then
  if INFLUX_TOKEN="$TOKEN" influx query --org "$ORG" \
      "from(bucket: \"$OTHER\") |> range(start: -1m) |> limit(n: 1)" >/dev/null 2>&1; then
    die "the new token can read bucket '$OTHER' - it is not scoped to '$BUCKET' alone"
  fi
  echo "    ok: read on '$BUCKET' allowed, read on '$OTHER' refused"
else
  echo "    ok: read on '$BUCKET' allowed (no other bucket to test refusal against)"
fi

# --- output ---------------------------------------------------------------
umask 077
printf 'influx_token: %s\n' "$TOKEN" > "$SECRETS_OUT"
chmod 600 "$SECRETS_OUT"

cat <<EOF

Done. Bucket:  $BUCKET ($BUCKET_ID), org $ORG, retention $RETENTION
Token written: $SECRETS_OUT (mode 600) - keep it out of git.

In metricsd.yaml:

  metricsd:
    influx:
      url: ${INFLUX_HOST:-http://<influx-host>:8086}
      org: $ORG
      bucket: $BUCKET
      token: !secret influx_token

Next: put $SECRETS_OUT beside metricsd.yaml as metricsd.secrets.yaml.
EOF
