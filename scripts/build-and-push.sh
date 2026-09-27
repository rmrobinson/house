#!/usr/bin/env bash
# Build (via Bazel, cross-compiling for both h031/amd64 and iot001/arm64 —
# see //images/README.md) and push every deployable bridge/service image to
# the self-hosted registry.
#
# Usage:
#   scripts/build-and-push.sh [bridge...]
#
# With no arguments, builds/pushes every image in BRIDGES below. Pass one or
# more names (matching //images:<name>_push) to push a subset, e.g.:
#   scripts/build-and-push.sh plex housed
#
# The registry is fronted by Caddy terminating real TLS from step-ca, so
# pushing just needs this machine to trust step-ca's root in its OS
# certificate store — see house-config/docker-compose/README.md for the
# registry's own setup and what each pulling host still needs configured.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Keep in sync with images/BUILD.bazel.
BRIDGES=(
  plex webos cast frigate omada ecobee housed bridgefacaded
  esphome airthings tesla-charger zwave zigbee
  apc-ups
)

TARGETS=("$@")
if [ ${#TARGETS[@]} -eq 0 ]; then
  TARGETS=("${BRIDGES[@]}")
fi

TAG="$(git rev-parse --short HEAD)"

for name in "${TARGETS[@]}"; do
  echo "==> //images:${name}_push"
  bazel run "//images:${name}_push" -- --tag "$TAG" --tag latest
done
