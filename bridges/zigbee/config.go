package main

// zigbeeConfig is the top-level bridge config, loaded from the "zigbee" viper key.
type zigbeeConfig struct {
	MQTT mqttConfig `mapstructure:"mqtt"`

	// Devices are per-physical-device config overrides, keyed by IEEEAddress. Unlike
	// bridges/zwave's per-product-model overrides (needed because Z-Wave's Color Switch CC
	// propertyKey numbering isn't standardized), zigbee2mqtt's exposes API already resolves that
	// kind of ambiguity for every device model via zigbee-herdsman-converters, so this bridge
	// needs far less config-override machinery - these entries exist only for cosmetic/identity
	// overrides, not to make a device buildable at all.
	Devices []deviceOverride `mapstructure:"devices"`
}

// deviceOverride is a bridge-config escape hatch for one physical device.
type deviceOverride struct {
	// IEEEAddress is the "0x..."-formatted IEEE address (zigbee2mqtt's stable identifier for a
	// device, unlike FriendlyName which a user can rename at any time) this override applies to.
	IEEEAddress string `mapstructure:"ieee_address"`

	// ID overrides the auto-assigned house device id ("zigbee-<ieeeAddress, 0x stripped>") for
	// this device. Left empty, the auto-assigned id is used.
	ID string `mapstructure:"id"`
}

// overrideFor returns the configured override for ieeeAddress, or a zero-value deviceOverride
// (every field empty) if none is configured - callers treat that the same as "use the
// auto-assigned id".
func (c zigbeeConfig) overrideFor(ieeeAddress string) deviceOverride {
	for _, o := range c.Devices {
		if o.IEEEAddress == ieeeAddress {
			return o
		}
	}
	return deviceOverride{}
}
