# monop-amp

This bridge interfaces with a Monoprice stero amplifier.

At the moment, this bridge queries the state of the amplifier at startup and then doesn't refresh it. I have seen issues with the amp not properly sending updates over the serial bus, so it's easiest to avoid the potential for corrupt data and just to execute the received commands.

## Raspberry Pi 1 (ARMv6) build

```
bazel build --config=rpi1 //bridges/monop-amp
```

produces a static, pure-Go `linux/arm` GOARM=6 binary at
`bazel-bin/bridges/monop-amp/monop-amp_/monop-amp`. Copy it to the Pi alongside a
`monoprice-amp.yaml` (see `monoprice-amp.example.yaml`; looked up in `/etc/house`,
`$HOME/.config/house`, then the working directory). The user running it needs access to the
serial device (e.g. the `dialout` group for `/dev/ttyUSB0`).
