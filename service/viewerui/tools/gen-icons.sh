#!/bin/bash
# Regenerates pwa/icons/*.png from tools/icon.svg. Needs macOS (sips) and Chrome
# for the one SVG->PNG render; the PNGs are checked in, so this only runs when
# the icon artwork changes.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
out="$here/../pwa/icons"
chrome="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

printf '<html><body style="margin:0;background:#0b0d0c"><img src="file://%s" width="512" height="512" style="display:block"></body></html>' \
  "$here/icon.svg" > "$tmp/icon.html"
"$chrome" --headless=new --disable-gpu --window-size=512,512 --hide-scrollbars \
  --screenshot="$tmp/icon-512.png" "file://$tmp/icon.html" >/dev/null 2>&1

cp "$tmp/icon-512.png" "$out/icon-512.png"
sips -z 192 192 "$tmp/icon-512.png" --out "$out/icon-192.png" >/dev/null
sips -z 180 180 "$tmp/icon-512.png" --out "$out/apple-touch-icon.png" >/dev/null
