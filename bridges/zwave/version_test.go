package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
)

func TestComputeVersion_GenericOnOff_ChangesWithState(t *testing.T) {
	on := &device.Device{Details: &device.Device_Generic{Generic: &device.Generic{
		OnOff: &trait.OnOff{State: &trait.OnOff_State{IsOn: true}},
	}}}
	off := &device.Device{Details: &device.Device_Generic{Generic: &device.Generic{
		OnOff: &trait.OnOff{State: &trait.OnOff_State{IsOn: false}},
	}}}

	assert.NotEqual(t, computeVersion(on), computeVersion(off))
	assert.Equal(t, computeVersion(on), computeVersion(on), "version must be deterministic for identical state")
}

func TestComputeVersion_Light_ChangesWithBrightnessAndColour(t *testing.T) {
	base := func(level int32, rgb *trait.Colour_State_RGB) *device.Device {
		return &device.Device{Details: &device.Device_Light{Light: &device.Light{
			OnOff:      &trait.OnOff{State: &trait.OnOff_State{IsOn: true}},
			Brightness: &trait.Brightness{State: &trait.Brightness_State{Level: level}},
			Colour:     &trait.Colour{State: &trait.Colour_State{Rgb: rgb}},
		}}}
	}

	d50 := base(50, &trait.Colour_State_RGB{Red: 1, Green: 2, Blue: 3})
	d75 := base(75, &trait.Colour_State_RGB{Red: 1, Green: 2, Blue: 3})
	dColour := base(50, &trait.Colour_State_RGB{Red: 9, Green: 2, Blue: 3})

	assert.NotEqual(t, computeVersion(d50), computeVersion(d75), "a brightness change must change the version")
	assert.NotEqual(t, computeVersion(d50), computeVersion(dColour), "a colour change must change the version")
}

// TestComputeVersion_Sensor_IgnoresTelemetry confirms a Sensor device's version is unaffected by
// its (entirely read-only - sensorBuilder.applyCommand always rejects every command) telemetry
// fields, per Device.version's documented contract of only reflecting command-target fields.
func TestComputeVersion_Sensor_IgnoresTelemetry(t *testing.T) {
	quiet := &device.Device{Details: &device.Device_Sensor{Sensor: &device.Sensor{
		Presence: &trait.Presence{State: &trait.Presence_State{MotionDetected: false}},
	}}}
	motion := &device.Device{Details: &device.Device_Sensor{Sensor: &device.Sensor{
		Presence: &trait.Presence{State: &trait.Presence_State{MotionDetected: true}},
	}}}

	assert.Equal(t, computeVersion(quiet), computeVersion(motion), "a Sensor has no command-target fields, so telemetry changes must not move its version")
}

func TestComputeVersion_Config_ChangesVersion(t *testing.T) {
	named := &device.Device{Config: &device.Device_Config{Name: "kitchen plug"}}
	renamed := &device.Device{Config: &device.Device_Config{Name: "hallway plug"}}

	assert.NotEqual(t, computeVersion(named), computeVersion(renamed))
}

// TestComputeVersion_IgnoresAddress confirms reachability/address telemetry - explicitly excluded
// by Device.version's documented contract - doesn't move the version, since a flapping connection
// would otherwise invalidate every in-flight client's version on every reconnect.
func TestComputeVersion_IgnoresAddress(t *testing.T) {
	reachable := &device.Device{
		Address: &device.Device_Address{IsReachable: true},
		Details: &device.Device_Generic{Generic: &device.Generic{OnOff: &trait.OnOff{State: &trait.OnOff_State{IsOn: true}}}},
	}
	unreachable := &device.Device{
		Address: &device.Device_Address{IsReachable: false},
		Details: &device.Device_Generic{Generic: &device.Generic{OnOff: &trait.OnOff{State: &trait.OnOff_State{IsOn: true}}}},
	}

	assert.Equal(t, computeVersion(reachable), computeVersion(unreachable))
}
