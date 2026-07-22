# AGENTS.md

Guidance for AI coding agents working in this repository.

## What this project is

`house` is a home-automation device abstraction layer, unrelated to (and simpler
in scope than) HomeAssistant/OpenHAB-style systems. See `README.md` for the
full vision; the short version:

- **`house`** (`service/house`) is the central service. It knows about the
  physical layout of a home (buildings, rooms) and which devices live where.
  It has no direct knowledge of device protocols.
- **`bridges`** (`bridges/*`) are standalone services, one per third-party
  system/protocol (Roku, Tesla wall charger, Frigate NVR, Omada SDN
  controller, APC UPS, Airthings BLE sensors, a Raspberry Pi clock, etc.).
  Each bridge translates between its native protocol and the `house` API
  contract, and exposes a gRPC service so `house` (or any client) can read
  device state and send commands to it.
- **`api`** (`api/*`) contains the protobuf contract shared by everything:
  `Bridge`, `Device`, `Command`, and per-capability `trait`s (OnOff,
  Brightness, Thermostat, Battery, NetworkPresence, ...). A device is
  composed of the traits it supports; traits split into `attributes`
  (what's possible) and `state` (what's currently true).
- **`clients/bridgecli`** is a cobra-based CLI for talking to a bridge's gRPC
  API directly (useful for manual testing of a bridge).

An automation engine and UI layer are described in the vision doc but live in
separate repos not present here — don't go looking for them in this tree.

## Build system: Bazel + Gazelle (Go modules are secondary)

This is a Bazel workspace (bzlmod, `MODULE.bazel`), not a plain Go module,
even though `go.mod`/`go.sum` exist and are kept in sync for gopls/editor
support (see `.vscode/settings.json`, which points gopls at the Bazel-managed
SDK via `tools/gopackagesdriver.sh`). **Treat Bazel as the source of truth
for building and testing.**

```sh
bazel build //...                      # build everything
bazel build //bridges/roku/...         # build one bridge
bazel test  //...                      # run all tests
bazel test  //service/bridge/...       # run one package's tests
bazel run   //:gazelle                 # regenerate BUILD.bazel files after
                                        # adding/removing/renaming .go files
                                        # or changing imports
```

Notes:
- After adding a new Go file, new import, or new package, run
  `bazel run //:gazelle` rather than hand-editing `BUILD.bazel` deps — the
  existing `BUILD.bazel` files are gazelle-generated (comments like
  `#keep` and `#gazelle:exclude empty.go` mark the few manual exceptions).
- New third-party Go dependencies: add to `go.mod` first (`go get ...`),
  then add the corresponding `com_github_...`/`org_...` repo name to the
  `use_repo(go_deps, ...)` list in `MODULE.bazel`, then re-run gazelle.
  `MODULE.bazel.lock` is generated — don't hand-edit it.
- `bazel-bin`, `bazel-house`, `bazel-out`, `bazel-testlogs` at the repo root
  are symlinks into the Bazel cache, gitignored. Never edit through them.
- Each `api/*` package (`api`, `api/command`, `api/device`, `api/trait`)
  has an `empty.go` containing just `package <name>` — this exists solely so
  `gopls`/`go build` treat the directory as a valid Go package before/without
  the proto-generated code; gazelle is told to exclude it via
  `#gazelle:exclude empty.go`. Leave this pattern alone when adding new
  proto-only packages.
- There is no CI configured in this repo (no `.github/workflows`). Running
  `bazel build //...` / `bazel test //...` locally is the only verification
  signal — do it before calling work done.
- `bridges/airthings` only builds on Linux
  (`target_compatible_with = ["@platforms//os:linux"]`, due to the tinygo
  BLE stack). Don't be surprised if it's skipped/fails on macOS — that's
  expected, not a regression.
- `.bazelrc` has a `--config=rpi1` for cross-compiling to ARMv6 (Raspberry
  Pi 1), but the `//:linux_arm6` platform it references isn't actually
  defined anywhere in the tree yet. Treat that config as aspirational/WIP,
  not a working path, unless you're specifically asked to finish it.

## Adding a new bridge

Look at `bridges/example` first — its `README.md` says explicitly it exists
to demonstrate bridge usage patterns. `bridges/omada` and
`bridges/tesla-charger` are good real-world references (poll-based HTTP
APIs); `bridges/example` shows the signal-driven / event-based shape.

The shape is consistent across every bridge:
1. `main.go`: set up zap logger, load config via viper (config name matches
   the bridge, search paths are `/etc/house`, `$HOME/.config/house`, `.`;
   see the `*.example.yaml` in each bridge dir for the expected shape),
   generate+persist a `bridge.id` UUID on first run if missing, construct
   `bridge.NewService(logger)`, construct the bridge type, call
   `svc.RegisterHandler(handlerImpl, bridgeProto)`, start the handler's
   `Run(ctx)` loop (goroutine), then `bridge.NewServer(logger, svc).ServeOnPort(...)`.
2. A type implementing `bridge.Handler` (`service/bridge/service.go`):
   `SetBridgeConfig`, `ProcessCommand`, `Refresh`. Poll-based bridges also
   run a ticker loop calling `Refresh` on an interval and pushing changes via
   `svc.UpdateDevice(...)`; event-based bridges push updates as events
   arrive.
3. Call `svc.UpdateDevice`/`svc.UpdateBridge`/`svc.RemoveDevice` whenever
   real state changes — `Service` diffs against its cache
   (`proto.Equal`) and only fans out an `Update` to subscribers when
   something actually changed, so it's safe (if slightly wasteful) to call
   these liberally.
4. New bridge directory needs its own `BUILD.bazel` (`go_library` +
   `go_binary`, generate with gazelle), a `README.md`, and a
   `<name>.example.yaml` showing the config shape.

## Proto/API conventions (`api/`)

- Package layout: `api` (top-level Bridge/House/Update contracts),
  `api/device` (device *shapes*, e.g. `Light`, `Thermostat`, composed of
  traits), `api/command` (things you can *send* to a device), `api/trait`
  (reusable capability building blocks, each with an `Attributes` message
  and a `State` message). Read `api/README.md` and `api/trait/README.md`
  before adding new device types or traits.
- A trait's `Attributes` describes what's *possible*; its `State` describes
  what's *currently true*. Keep new traits consistent with this split.
- Proto files use `syntax = "proto3"`, package `faltung.house.api.<dir>`,
  and an explicit `go_package` option matching the Go import path.
- Errors returned from gRPC-facing code should be `google.golang.org/grpc/status`
  errors with an appropriate `codes.*`, not bare `errors.New`. Bridge-level
  sentinel errors live in `service/bridge/error.go`
  (`ErrUnsupportedCommand`, `ErrInvalidTimezone`, ...) — reuse those from
  `Handler.ProcessCommand` implementations rather than inventing new ones
  per bridge unless the situation genuinely differs.

## Go code conventions

- Logging: `go.uber.org/zap`, always structured
  (`logger.Error("unable to X", zap.Error(err), zap.String("device_id", id))`),
  never `fmt.Print*`/`log.*` in library or service code. `zap.NewDevelopment()`
  in `main()`, `zaptest.NewLogger(t)` in tests.
- Config: `github.com/spf13/viper` for bridge config (YAML), read via
  `viper.GetString`/`GetInt`/etc. and persisted back with `viper.WriteConfig()`
  when a bridge needs to save generated state (e.g. a new bridge ID). CLIs
  use `github.com/spf13/cobra` (see `clients/bridgecli/cmd`).
- Protobuf messages: treat them as owned by whoever last called
  `proto.Clone` — `service/bridge/service.go`'s `Service` clones on both
  write (`UpdateDevice`) and read (`getDevice`/`getDevices`) so callers
  never get a reference that internal state can mutate out from under them.
  Preserve this pattern in new code that caches proto messages.
- Import grouping (goimports, local prefix `github.com/rmrobinson/house`
  per `.vscode/settings.json`): stdlib, then third-party, then
  `github.com/rmrobinson/house/...` last, each in its own blank-line-separated
  block. `gofumpt` is the configured formatter in editor settings; if it's
  not installed, plain `gofmt` plus matching the existing import grouping by
  hand is the fallback.
- The root `api` package is conventionally imported as `api2` (e.g.
  `api2 "github.com/rmrobinson/house/api"`) to leave `api` free for a local
  variable/param name and to disambiguate from `api/device`, `api/command`,
  etc. imported alongside it. Follow this alias when a file imports the root
  `api` package together with its subpackages.
- Comments: exported types/functions get a doc comment; avoid narrating
  obvious code. `// TODO:` comments are used in this codebase to flag known
  gaps (e.g. a race condition in `service/bridge/api.go`'s `StreamUpdates`)
  — check nearby TODOs before "fixing" something that's actually a known,
  deliberately-deferred issue, and don't silently delete a TODO without
  actually resolving what it describes.

## Testing

- Standard `go test` via Bazel (`bazel test //...`), using
  `github.com/stretchr/testify/assert` and `zaptest.NewLogger(t)`.
- For bridges that call an HTTP API, stub it with `net/http/httptest` rather
  than mocking the client (see `bridges/tesla-charger/charger_test.go`).
- `service/bridge/source_test.go` shows the pattern for testing the
  concurrent pub/sub `Source`/`Sink` type — heavy use of goroutines +
  `sync.WaitGroup`, asserting on final state after `Wait()`, not on
  intermediate timing.

## House service persistence (`service/house/db`)

- SQLite via `github.com/mattn/go-sqlite3`, migrations via
  `golang-migrate/migrate/v4`, embedded with `//go:embed migrations/*.sql`
  and run automatically in `db.NewDatabase`. Add new migrations as a new
  numbered pair (`00000N_description.up.sql` / `.down.sql`) in
  `service/house/db/migrations` — never edit an already-numbered migration
  that could have run against someone's existing DB.

## Git / commit conventions

- Commit subjects are short, imperative, present-tense (`"Add the /etc
  config path"`, `"Cleanup and standardize bridges"`) — no conventional-commit
  prefixes in use.
- History shows a feature-branch-per-change + PR-merge workflow even though
  this is a personal repo (`Merge pull request #16 from
  rmrobinson/standardize-bridge-configs`). Follow that pattern rather than
  committing straight to `main` unless told otherwise.
- Don't commit `TODO.md` changes casually — it's the author's own running
  checklist of sensor/config integration work; only touch it if asked to or
  if you complete an item it lists.
