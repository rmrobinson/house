package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func validConfig() ecobeeConfig {
	return ecobeeConfig{
		AccessoryName: "ecobee",
		Thermostat:    houseDeviceConfig{ID: "ecobee-main", Name: "Main Floor Thermostat"},
		Sensors: []sensorConfig{
			{ID: "ecobee-downstairs", Name: "Downstairs", AccessoryID: 4297248826},
		},
	}
}

func TestConfigValidateAcceptsValidConfig(t *testing.T) {
	assert.NoError(t, validConfig().validate())
}

func TestConfigValidateRejectsMissingThermostatID(t *testing.T) {
	cfg := validConfig()
	cfg.Thermostat.ID = ""
	assert.Error(t, cfg.validate())
}

func TestConfigValidateRejectsMissingThermostatName(t *testing.T) {
	cfg := validConfig()
	cfg.Thermostat.Name = ""
	assert.Error(t, cfg.validate())
}

func TestConfigValidateRejectsIncompleteSensor(t *testing.T) {
	for _, mutate := range []func(*sensorConfig){
		func(s *sensorConfig) { s.ID = "" },
		func(s *sensorConfig) { s.Name = "" },
		func(s *sensorConfig) { s.AccessoryID = 0 },
	} {
		cfg := validConfig()
		mutate(&cfg.Sensors[0])
		assert.Error(t, cfg.validate())
	}
}

func TestConfigValidateRejectsSensorIDCollidingWithThermostat(t *testing.T) {
	cfg := validConfig()
	cfg.Sensors[0].ID = cfg.Thermostat.ID
	assert.Error(t, cfg.validate())
}

func TestConfigValidateRejectsDuplicateSensorIDs(t *testing.T) {
	cfg := validConfig()
	cfg.Sensors = append(cfg.Sensors, sensorConfig{ID: cfg.Sensors[0].ID, Name: "Duplicate", AccessoryID: 999})
	assert.Error(t, cfg.validate())
}
