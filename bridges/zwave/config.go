package main

// zwaveConfig is the top-level bridge config, loaded from the "zwave" viper key.
type zwaveConfig struct {
	MQTT mqttConfig `mapstructure:"mqtt"`

	// Devices are per-product-model config overrides, keyed by DeviceID
	// ("<manufacturerId>-<productId>-<producttype>", stable per physical product - see nodeInfo).
	// Unlike bridges/esphome's per-node `roles:` config, Z-Wave CC/property naming is
	// standardized enough that no per-device role mapping is needed; these entries exist only
	// for the handful of things the standard can't infer - see each field's doc comment.
	Devices []deviceOverride `mapstructure:"devices"`
}

// deviceOverride is a bridge-config escape hatch for one product model the standard
// deviceClass-driven convention table can't fully resolve on its own.
type deviceOverride struct {
	// DeviceID is the "<manufacturerId>-<productId>-<producttype>" key this override applies to.
	DeviceID string `mapstructure:"device_id"`

	// ID overrides the auto-assigned house device id ("zwave-<nodeId>") for every node matching
	// DeviceID. Left empty, the auto-assigned id is used.
	ID string `mapstructure:"id"`

	// BinarySwitchLabel overrides classifyBinarySwitchLabel's description/label keyword match
	// (see the design doc's "Disambiguating device role from device class" section) for a
	// specific product this bridge account owns that the keyword match gets wrong. One entry per
	// distinct product, not per node.
	BinarySwitchLabel string `mapstructure:"binary_switch_label"`

	// ColourChannels maps a Colour Switch (CC 51) channel name ("red", "green", "blue",
	// "warm_white", "cold_white") to its propertyKey on this product model, e.g. {"red": "2",
	// "green": "3", "blue": "4"}. Required for rgbLightBuilder - per the design doc, the
	// propertyKey numbering for color components is not fixed across devices, so this bridge
	// deliberately does not guess a default numbering from a datasheet. A bulb with no
	// ColourChannels entry for its DeviceID fails to build rather than silently writing to the
	// wrong channel.
	ColourChannels map[string]string `mapstructure:"colour_channels"`

	// ColourTargetProperty/ColourCurrentProperty override CC 51's write/read property names
	// ("targetColor"/"currentColor" by default) for a product whose zwave-js-ui integration uses
	// different naming. This is exactly as unverified an assumption as ColourChannels' propertyKey
	// numbers above - it just has a reasonable-guess default instead of failing closed, since a
	// role table needs *some* property name to exist at all. If colour commands against a bulb
	// consistently time out despite a correct ColourChannels mapping, this is the next thing to
	// check - confirm the actual property name against the bulb's own nodeinfo/valueId list and
	// override it here.
	ColourTargetProperty  string `mapstructure:"colour_target_property"`
	ColourCurrentProperty string `mapstructure:"colour_current_property"`
}

// overrideFor returns the configured override for deviceID, or a zero-value deviceOverride
// (every field empty) if none is configured - callers treat that the same as "use the standard
// convention table for everything".
func (c zwaveConfig) overrideFor(deviceID string) deviceOverride {
	for _, o := range c.Devices {
		if o.DeviceID == deviceID {
			return o
		}
	}
	return deviceOverride{}
}
