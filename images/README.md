# images

Packages `house` binaries as multi-arch OCI images and pushes them to the
self-hosted registry used by `house-config`'s docker-compose deploys
(`h031:5000`, see that repo's `docker-compose/README.md`).

Each bridge/service gets one `bridge_image(name, binary)` call in
`BUILD.bazel` (see `defs.bzl`), which produces:

- `<name>_image` — an `oci_image` built from the `go_binary` at `binary`,
  tarred into `/usr/local/bin/<name>`.
- `<name>_platform_images` / `<name>_index` — the same image built for both
  `//platforms:linux_amd64` and `//platforms:linux_arm64` via a Bazel platform
  transition (`transition.bzl`, copied from rules_oci's own
  `examples/multi_architecture_image` — rules_oci doesn't ship this as a rule,
  it's the documented pattern for consumers), combined into one
  `oci_image_index` manifest list.
- `<name>_push` — pushes that index to `h031:5000/house/<name>`.

## Cross-compilation

`MODULE.bazel` registers `hermetic_cc_toolchain`'s Zig-based glibc toolchains
for both platforms, so CGO binaries (`airthings` today; `housed` via
`mattn/go-sqlite3`) cross-compile cleanly — no QEMU, no hand-maintained
sysroot. Pure-Go bridges cross-compile via `rules_go`'s native GOOS/GOARCH
support regardless; the toolchain only matters for the CGO ones.

## Base images

`@distroless_base` (glibc, no shell) is the default. `airthings` uses
`@debian_slim` instead — it needs `libdbus` at runtime (tinygo BLE's Linux
backend talks to BlueZ over D-Bus via `cbgo`), which distroless/base doesn't
carry.

## Building and pushing

Use `scripts/build-and-push.sh` from the repo root rather than calling Bazel
directly — see that script for the exact commands. It pushes with `--insecure`
since the registry is plain HTTP.

## Pending

`zwave` and `facade` aren't wired up here yet — see the commented-out entries
at the bottom of `BUILD.bazel`. Both reference packages that only exist on
unmerged branches (`worktree-add-zwave-bridge`, `add-bridge-configs`); uncomment
once those land on `main`.
