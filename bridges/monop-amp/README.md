# monop-amp

This bridge interfaces with a Monoprice stero amplifier.

The bridge reads the state of every active zone at startup, then re-reads them every `bridge.refresh_interval` seconds (default 60; 0 disables) so that changes made at a wall keypad show up. A zone that fails to refresh (for example, a serial read timing out) is published as unreachable and is retried on the next refresh, without affecting the other zones.

Only `active` speakers and inputs in the config are published or accepted by commands.

## Raspberry Pi 1 (ARMv6) build

```
bazel build --config=rpi1 //bridges/monop-amp
```

produces a static, pure-Go `linux/arm` GOARM=6 binary at
`bazel-bin/bridges/monop-amp/monop-amp_/monop-amp`. Copy it to the Pi alongside a
`monoprice-amp.yaml` (see `monoprice-amp.example.yaml`; looked up in `/etc/house`, then the
working directory). The user running it needs access to the serial device (e.g. the `dialout`
group for `/dev/ttyUSB0`), and the config's *directory* must be writable by that user: the bridge
saves its generated `bridge.id` and speaker last-seen times by writing a temp file next to the
config and renaming it into place.
