package main

import "fmt"

// ecobeeConfig is the top-level "ecobee" key in the bridge's config.yaml.
type ecobeeConfig struct {
	// PairingStore is the path to the JSON pairing store written by cmd/pair.
	PairingStore string `mapstructure:"pairing_store"`
	// AccessoryName is the alias cmd/pair saved the ecobee's pairing under (its --name flag).
	// Defaults to "ecobee" if unset.
	AccessoryName string `mapstructure:"accessory_name"`

	Thermostat houseDeviceConfig `mapstructure:"thermostat"`
	Sensors    []sensorConfig    `mapstructure:"sensors"`
}

// houseDeviceConfig names a house device this bridge exposes.
type houseDeviceConfig struct {
	ID   string `mapstructure:"id"`
	Name string `mapstructure:"name"`
}

// sensorConfig is a remote sensor paired alongside the thermostat, exposed as its own house
// device.Sensor. AccessoryID is the HAP aid it was assigned at pairing time (see
// docs/ecobee-hap-dump.txt) - not something this bridge can discover on its own without also
// implementing the /accessories descriptor fetch, which config-driven aids avoid needing.
type sensorConfig struct {
	ID          string `mapstructure:"id"`
	Name        string `mapstructure:"name"`
	AccessoryID uint64 `mapstructure:"accessory_id"`
}

// validate catches config mistakes at startup rather than letting them silently produce a broken
// or colliding device ID later (an empty thermostat.id, for instance, would otherwise build and
// publish a device with Id: "" without error).
func (c ecobeeConfig) validate() error {
	if c.Thermostat.ID == "" {
		return fmt.Errorf("ecobee.thermostat.id must be set")
	}
	if c.Thermostat.Name == "" {
		return fmt.Errorf("ecobee.thermostat.name must be set")
	}

	seen := map[string]string{c.Thermostat.ID: "thermostat"}
	for i, s := range c.Sensors {
		if s.ID == "" {
			return fmt.Errorf("ecobee.sensors[%d].id must be set", i)
		}
		if s.Name == "" {
			return fmt.Errorf("ecobee.sensors[%d].name must be set", i)
		}
		if s.AccessoryID == 0 {
			return fmt.Errorf("ecobee.sensors[%d].accessory_id must be set", i)
		}
		if owner, exists := seen[s.ID]; exists {
			return fmt.Errorf("ecobee.sensors[%d].id %q collides with %s", i, s.ID, owner)
		}
		seen[s.ID] = fmt.Sprintf("sensors[%d]", i)
	}
	return nil
}
