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
# Pushes with --insecure since the registry (h031:5000) is plain HTTP — see
# house-config/docker-compose/README.md for the registry's own setup and the
# insecure-registries daemon config each pulling host needs.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Keep in sync with images/BUILD.bazel.
BRIDGES=(
  plex webos cast frigate omada ecobee housed facade
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
  bazel run "//images:${name}_push" -- --insecure --tag "$TAG" --tag latest
done
