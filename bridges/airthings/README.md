# airthings bridge

Polls compatible Airthings Bluetooth sensors and reports on the air quality.

## History

Each sensor keeps its own on-board log, and the bridge serves it through
`BridgeService.GetDeviceHistory` (see `bridge.HistoryHandler`), so a caller can
poll rarely and fill the gaps afterwards. Try it with
`bridgecli device history --deviceID <serial> --within 6h`.

- Samples are 5 minutes apart; the sensor reports them in hourly blocks, with
  one radon average pair per block.
- The sensor can only be asked for "the last N hours", so a single call reaches
  at most 24h back (advertised as `Sensor.Metadata.history.max_window`); anything
  older is unrecoverable. Call it more often than that.
- A history download holds the BLE adapter for up to 30s and is serialised with
  the regular refresh, so it can briefly delay a live reading.
- History carries no battery level, and the sensor's illuminance isn't mapped
  anywhere: it is a percentage, whereas `LightLevel` is in lux.

## Notes

Due to limitations on the underlying tinygo Bluetooth LE implementation, it currently only builds for Linux.