#!/usr/bin/env bash
# Build (via Bazel, cross-compiling for both amd64 and arm64 — see
# //images/README.md) and push every deployable bridge/service image to
# a registry.
#
# Usage:
#   HOUSE_REGISTRY=<registry-host> scripts/build-and-push.sh [bridge...]
#
# HOUSE_REGISTRY is required (e.g. "registry.example.com" or
# "index.docker.io/<user>") — this repo has no default registry baked in;
# see //images/README.md. Images are pushed to
# "$HOUSE_REGISTRY/house/<bridge>".
#
# With no positional arguments, builds/pushes every image in BRIDGES below.
# Pass one or more names (matching //images:<name>_push) to push a subset,
# e.g.:
#   HOUSE_REGISTRY=registry.example.com scripts/build-and-push.sh plex housed
#
# Whatever fronts your registry needs to be trusted by this machine
# (standard `docker login`/OS cert-store trust, depending on setup) — that's
# entirely deployment-specific and not this repo's concern.
set -euo pipefail

: "${HOUSE_REGISTRY:?set HOUSE_REGISTRY to the registry host to push images to, e.g. HOUSE_REGISTRY=registry.example.com $0}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Keep in sync with images/BUILD.bazel.
BRIDGES=(
  plex webos cast frigate omada ecobee housed bridgefacaded
  esphome airthings tesla-charger zwave zigbee
  apc-ups nanoleaf raspi-clock roku example
  policyd adminui viewerui
)

TARGETS=("$@")
if [ ${#TARGETS[@]} -eq 0 ]; then
  TARGETS=("${BRIDGES[@]}")
fi

TAG="$(git rev-parse --short HEAD)"

for name in "${TARGETS[@]}"; do
  echo "==> //images:${name}_push -> $HOUSE_REGISTRY/house/${name}"
  bazel run "//images:${name}_push" -- \
    --repository "$HOUSE_REGISTRY/house/${name}" --tag "$TAG" --tag latest
done
