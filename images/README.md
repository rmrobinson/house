# images

Packages `house` binaries as multi-arch OCI images and pushes them to
whatever registry the caller specifies — this repo has no registry address
baked in; see "Building and pushing" below.

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
- `<name>_push` — pushes that index to `<registry>/house/<name>`, where
  `<registry>` must be supplied at `bazel run` time (see below) — `defs.bzl`
  deliberately leaves `oci_push`'s `repository` attribute unset, which is
  rules_oci's own supported way to require the caller to pass
  `--repository` rather than bake in a default.

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
directly — it requires a `HOUSE_REGISTRY` env var (no default) and passes it
to each `<name>_push` target as `--repository "$HOUSE_REGISTRY/house/<name>"`;
see that script's header for usage. Calling `bazel run //images:<name>_push`
directly works the same way — pass `-- --repository <host>/house/<name>`.

Whatever fronts your registry (real TLS, `--insecure`, registry auth, etc.)
is entirely deployment-specific and outside this repo's concern — configure
your own trust/auth however your registry needs it.
