package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"

	action "github.com/rmrobinson/google-smart-home-action-go"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
)

func hsbLight(id, name string, on bool, brightness, hue, saturation, brightnessPct int32) *device.Device {
	return &device.Device{
		Id:     id,
		Config: &device.Device_Config{Name: name},
		Details: &device.Device_Light{Light: &device.Light{
			OnOff: &trait.OnOff{
				Attributes: &trait.OnOff_Attributes{CanControl: true},
				State:      &trait.OnOff_State{IsOn: on},
			},
			Brightness: &trait.Brightness{
				Attributes: &trait.Brightness_Attributes{CanControl: true},
				State:      &trait.Brightness_State{Level: brightness},
			},
			Colour: &trait.Colour{
				Attributes: &trait.Colour_Attributes{CanControl: true, Mode: trait.Colour_Attributes_MODE_HSB},
				State: &trait.Colour_State{Hsb: &trait.Colour_State_HSB{
					Hue: hue, Saturation: saturation, Brightness: brightnessPct,
				}},
			},
		}},
	}
}

func TestDeviceToAction_Light(t *testing.T) {
	d := hsbLight("light1", "Kitchen Light", true, 42, 120, 50, 80)

	ad, ok := deviceToAction(d)
	assert.True(t, ok)
	assert.Equal(t, "light1", ad.ID)
	assert.Equal(t, "action.devices.types.LIGHT", ad.Type)
	assert.Equal(t, "Kitchen Light", ad.Name.Name)
	assert.True(t, ad.Traits["action.devices.traits.OnOff"])
	assert.True(t, ad.Traits["action.devices.traits.Brightness"])
	assert.True(t, ad.Traits["action.devices.traits.ColorSetting"])
	assert.Equal(t, action.HSV, ad.Attributes["colorModel"])
}

func TestDeviceToAction_FallsBackToIDWhenNameUnset(t *testing.T) {
	d := hsbLight("light1", "", true, 0, 0, 0, 0)
	ad, ok := deviceToAction(d)
	assert.True(t, ok)
	assert.Equal(t, "light1", ad.Name.Name)
}

func TestDeviceToAction_UnsupportedKind(t *testing.T) {
	d := &device.Device{Id: "sensor1", Details: &device.Device_Sensor{Sensor: &device.Sensor{}}}
	_, ok := deviceToAction(d)
	assert.False(t, ok)
}

func TestStateToAction_Light(t *testing.T) {
	d := hsbLight("light1", "Kitchen Light", true, 42, 120, 50, 80)

	ds, err := stateToAction(d)
	assert.NoError(t, err)
	assert.True(t, ds.Online)
	assert.Equal(t, true, ds.State["on"])
	assert.Equal(t, 42, ds.State["brightness"])

	color, ok := ds.State["color"].(map[string]interface{})
	assert.True(t, ok)
	hsv, ok := color["spectrumHsv"].(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, float64(120), hsv["hue"])
	assert.Equal(t, float64(50)/100.0, hsv["saturation"])
	assert.Equal(t, float64(80)/100.0, hsv["value"])
}

func TestStateToAction_UnsupportedKind(t *testing.T) {
	d := &device.Device{Id: "sensor1", Details: &device.Device_Sensor{Sensor: &device.Sensor{}}}
	_, err := stateToAction(d)
	assert.Error(t, err)
}

func TestBuildCommand_OnOff(t *testing.T) {
	d := hsbLight("light1", "Kitchen Light", false, 0, 0, 0, 0)
	cmd, ok := buildCommand(d, "light1", action.Command{OnOff: &action.CommandOnOff{On: true}})
	assert.True(t, ok)
	assert.Equal(t, "light1", cmd.DeviceId)
	onOff, ok := cmd.Details.(*command.Command_OnOff)
	assert.True(t, ok)
	assert.True(t, onOff.OnOff.On)
}

func TestBuildCommand_BrightnessAbsolute(t *testing.T) {
	d := hsbLight("light1", "Kitchen Light", true, 10, 0, 0, 0)
	cmd, ok := buildCommand(d, "light1", action.Command{BrightnessAbsolute: &action.CommandBrightnessAbsolute{Brightness: 77}})
	assert.True(t, ok)
	b, ok := cmd.Details.(*command.Command_BrightnessAbsolute)
	assert.True(t, ok)
	assert.Equal(t, int32(77), b.BrightnessAbsolute.BrightnessPercent)
}

func TestBuildCommand_ColorAbsoluteHSB(t *testing.T) {
	d := hsbLight("light1", "Kitchen Light", true, 10, 0, 0, 0)
	cmd := action.Command{ColorAbsolute: &action.CommandColorAbsolute{}}
	cmd.ColorAbsolute.Color.HSV.Hue = 180
	cmd.ColorAbsolute.Color.HSV.Saturation = 0.6
	cmd.ColorAbsolute.Color.HSV.Value = 0.9

	cc, ok := buildCommand(d, "light1", cmd)
	assert.True(t, ok)
	colour, ok := cc.Details.(*command.Command_Colour)
	assert.True(t, ok)
	hsb, ok := colour.Colour.Value.(*command.Colour_Hsb)
	assert.True(t, ok)
	assert.Equal(t, int32(180), hsb.Hsb.Hue)
	assert.Equal(t, int32(60), hsb.Hsb.Saturation)
	assert.Equal(t, int32(90), hsb.Hsb.Brightness)
}

func TestBuildCommand_UnsupportedCommand(t *testing.T) {
	d := hsbLight("light1", "Kitchen Light", true, 10, 0, 0, 0)
	_, ok := buildCommand(d, "light1", action.Command{SetVolume: &action.CommandSetVolume{Level: 10}})
	assert.False(t, ok)
}

func avReceiver(id, name string, on bool, volumeLevel int32, muted bool, currentInputID string) *device.Device {
	return &device.Device{
		Id:     id,
		Config: &device.Device_Config{Name: name},
		Details: &device.Device_AvReceiver{AvReceiver: &device.AVReceiver{
			OnOff: &trait.OnOff{State: &trait.OnOff_State{IsOn: on}},
			Volume: &trait.Volume{
				Attributes: &trait.Volume_Attributes{CanMute: true, MaximumLevel: 100},
				State:      &trait.Volume_State{Level: volumeLevel, IsMuted: muted},
			},
			Input: &trait.Input{
				Attributes: &trait.Input_Attributes{Inputs: []*trait.Input_InputDetails{
					{Id: "hdmi1", Name: "Blu-ray"},
					{Id: "hdmi2", Name: "Game Console"},
				}},
				State: &trait.Input_State{CurrentInputId: currentInputID},
			},
		}},
	}
}

func TestDeviceToAction_AVReceiver(t *testing.T) {
	d := avReceiver("avr1", "Living Room Receiver", true, 20, false, "hdmi1")

	ad, ok := deviceToAction(d)
	assert.True(t, ok)
	assert.Equal(t, "action.devices.types.AUDIO_VIDEO_RECEIVER", ad.Type)
	assert.True(t, ad.Traits["action.devices.traits.OnOff"])
	assert.True(t, ad.Traits["action.devices.traits.Volume"])
	assert.True(t, ad.Traits["action.devices.traits.InputSelector"])
	assert.Equal(t, 100, ad.Attributes["volumeMaxLevel"])
}

func TestStateToAction_AVReceiver(t *testing.T) {
	d := avReceiver("avr1", "Living Room Receiver", true, 20, false, "hdmi1")

	ds, err := stateToAction(d)
	assert.NoError(t, err)
	assert.Equal(t, true, ds.State["on"])
	assert.Equal(t, 20, ds.State["currentVolume"])
	assert.Equal(t, "hdmi1", ds.State["input"])
}

func TestBuildCommand_AVReceiver(t *testing.T) {
	d := avReceiver("avr1", "Living Room Receiver", true, 20, false, "hdmi1")

	cmd, ok := buildCommand(d, "avr1", action.Command{SetInput: &action.CommandSetInput{NewInput: "hdmi2"}})
	assert.True(t, ok)
	in, ok := cmd.Details.(*command.Command_Input)
	assert.True(t, ok)
	assert.Equal(t, "hdmi2", in.Input.InputId)

	cmd, ok = buildCommand(d, "avr1", action.Command{SetVolume: &action.CommandSetVolume{Level: 50}})
	assert.True(t, ok)
	vol, ok := cmd.Details.(*command.Command_VolumeAbsolute)
	assert.True(t, ok)
	assert.Equal(t, int32(50), vol.VolumeAbsolute.Level)

	_, ok = buildCommand(d, "avr1", action.Command{NextInput: &action.CommandNextInput{}})
	assert.False(t, ok)
}

func TestDeviceToAction_TelevisionPartial(t *testing.T) {
	d := &device.Device{
		Id: "tv1",
		Details: &device.Device_Television{Television: &device.Television{
			OnOff: &trait.OnOff{State: &trait.OnOff_State{IsOn: true}},
		}},
	}

	ad, ok := deviceToAction(d)
	assert.True(t, ok)
	assert.Equal(t, "action.devices.types.TV", ad.Type)
	assert.True(t, ad.Traits["action.devices.traits.OnOff"])
	assert.False(t, ad.Traits["action.devices.traits.Volume"])
}

func TestDeviceToAction_TelevisionEmpty(t *testing.T) {
	d := &device.Device{Id: "tv1", Details: &device.Device_Television{Television: &device.Television{}}}
	_, ok := deviceToAction(d)
	assert.False(t, ok)
}

func fan(id string, on bool, min, max, cur int32) *device.Device {
	return &device.Device{
		Id: id,
		Details: &device.Device_Fan{Fan: &device.Fan{
			OnOff: &trait.OnOff{State: &trait.OnOff_State{IsOn: on}},
			Speed: &trait.Speed{
				Attributes: &trait.Speed_Attributes{MinimumSpeed: min, MaximumSpeed: max},
				State:      &trait.Speed_State{CurrentSpeed: cur},
			},
		}},
	}
}

func TestDeviceToAction_Fan(t *testing.T) {
	d := fan("fan1", true, 0, 5, 3)
	ad, ok := deviceToAction(d)
	assert.True(t, ok)
	assert.Equal(t, "action.devices.types.FAN", ad.Type)
	assert.True(t, ad.Traits["action.devices.traits.FanSpeed"])
}

func TestStateToAction_Fan(t *testing.T) {
	d := fan("fan1", true, 0, 5, 3)
	ds, err := stateToAction(d)
	assert.NoError(t, err)
	assert.Equal(t, 60, ds.State["currentFanSpeedPercent"])
}

func TestBuildCommand_FanSpeed(t *testing.T) {
	d := fan("fan1", true, 0, 5, 3)
	pct := float32(40)
	cmd, ok := buildCommand(d, "fan1", action.Command{SetFanSpeed: &action.CommandSetFanSpeed{FanSpeedPercent: &pct}})
	assert.True(t, ok)
	sp, ok := cmd.Details.(*command.Command_Speed)
	assert.True(t, ok)
	assert.Equal(t, int32(2), sp.Speed.Value)
}

func thermostat(id string, targetMode trait.Thermostat_Mode, heatC, coolC, curC float32) *device.Device {
	return &device.Device{
		Id: id,
		Details: &device.Device_Thermostat{Thermostat: &device.Thermostat{
			Thermostat: &trait.Thermostat{
				Attributes: &trait.Thermostat_Attributes{
					SupportedModes: []trait.Thermostat_Mode{trait.Thermostat_OFF, trait.Thermostat_HEAT, trait.Thermostat_COOL, trait.Thermostat_AUTO},
				},
				State: &trait.Thermostat_State{
					TargetMode:                targetMode,
					HeatSetpointCelsius:       &heatC,
					CoolSetpointCelsius:       &coolC,
					CurrentTemperatureCelsius: curC,
				},
			},
		}},
	}
}

func TestDeviceToAction_Thermostat(t *testing.T) {
	d := thermostat("tstat1", trait.Thermostat_AUTO, 20, 24, 21)
	ad, ok := deviceToAction(d)
	assert.True(t, ok)
	assert.Equal(t, "action.devices.types.THERMOSTAT", ad.Type)
	assert.True(t, ad.Traits["action.devices.traits.TemperatureSetting"])
	assert.ElementsMatch(t, []string{"off", "heat", "cool", "heatcool"}, ad.Attributes["availableThermostatModes"])
}

func TestStateToAction_ThermostatAuto(t *testing.T) {
	d := thermostat("tstat1", trait.Thermostat_AUTO, 20, 24, 21)
	ds, err := stateToAction(d)
	assert.NoError(t, err)
	assert.Equal(t, "heatcool", ds.State["thermostatMode"])
	assert.Equal(t, float32(24), ds.State["thermostatTemperatureSetpointHigh"])
	assert.Equal(t, float32(20), ds.State["thermostatTemperatureSetpointLow"])
	assert.Equal(t, float32(21), ds.State["thermostatTemperatureAmbient"])
}

func TestBuildCommand_ThermostatSetpoint(t *testing.T) {
	d := thermostat("tstat1", trait.Thermostat_HEAT, 20, 24, 21)

	cmd, ok := buildCommand(d, "tstat1", action.Command{ThermostatTemperatureSetpoint: &action.CommandThermostatTemperatureSetpoint{
		ThermostatTemperatureSetpointCelcius: 22,
	}})
	assert.True(t, ok)
	st, ok := cmd.Details.(*command.Command_SetTemperature)
	assert.True(t, ok)
	assert.Equal(t, float32(22), st.SetTemperature.SetpointCelsius)
}

func TestBuildCommand_ThermostatSetMode(t *testing.T) {
	d := thermostat("tstat1", trait.Thermostat_AUTO, 20, 24, 21)

	cmd := action.Command{Generic: &action.CommandGeneric{
		Command: "action.devices.commands.ThermostatSetMode",
		Params:  map[string]interface{}{"thermostatMode": "cool"},
	}}
	cc, ok := buildCommand(d, "tstat1", cmd)
	assert.True(t, ok)
	m, ok := cc.Details.(*command.Command_SetThermostatMode)
	assert.True(t, ok)
	assert.Equal(t, trait.Thermostat_COOL, m.SetThermostatMode.Mode)
}

func TestBuildCommand_ThermostatSetModeUnrecognized(t *testing.T) {
	d := thermostat("tstat1", trait.Thermostat_AUTO, 20, 24, 21)
	cmd := action.Command{Generic: &action.CommandGeneric{
		Command: "action.devices.commands.ThermostatSetMode",
		Params:  map[string]interface{}{"thermostatMode": "nonsense"},
	}}
	_, ok := buildCommand(d, "tstat1", cmd)
	assert.False(t, ok)
}

func evCharger(id string, on bool) *device.Device {
	return &device.Device{
		Id: id,
		Details: &device.Device_EvCharger{EvCharger: &device.EVCharger{
			OnOff: &trait.OnOff{State: &trait.OnOff_State{IsOn: on}},
		}},
	}
}

func TestDeviceToAction_EVCharger(t *testing.T) {
	d := evCharger("ev1", true)
	ad, ok := deviceToAction(d)
	assert.True(t, ok)
	assert.Equal(t, "action.devices.types.CHARGER", ad.Type)
	assert.True(t, ad.Traits["action.devices.traits.OnOff"])
}

func TestBuildCommand_EVCharger(t *testing.T) {
	d := evCharger("ev1", false)
	cmd, ok := buildCommand(d, "ev1", action.Command{OnOff: &action.CommandOnOff{On: true}})
	assert.True(t, ok)
	onOff, ok := cmd.Details.(*command.Command_OnOff)
	assert.True(t, ok)
	assert.True(t, onOff.OnOff.On)

	_, ok = buildCommand(d, "ev1", action.Command{SetVolume: &action.CommandSetVolume{Level: 1}})
	assert.False(t, ok)
}

func TestBuildCommand_EVChargerNilSubMessage(t *testing.T) {
	d := &device.Device{Id: "ev1", Details: &device.Device_EvCharger{EvCharger: nil}}
	_, ok := buildCommand(d, "ev1", action.Command{OnOff: &action.CommandOnOff{On: true}})
	assert.False(t, ok)
}

func TestBuildCommand_ThermostatNilNestedTrait(t *testing.T) {
	d := &device.Device{Id: "tstat1", Details: &device.Device_Thermostat{Thermostat: &device.Thermostat{}}}

	_, ok := buildCommand(d, "tstat1", action.Command{ThermostatTemperatureSetpoint: &action.CommandThermostatTemperatureSetpoint{
		ThermostatTemperatureSetpointCelcius: 22,
	}})
	assert.False(t, ok)

	cmd := action.Command{Generic: &action.CommandGeneric{
		Command: "action.devices.commands.ThermostatSetMode",
		Params:  map[string]interface{}{"thermostatMode": "cool"},
	}}
	_, ok = buildCommand(d, "tstat1", cmd)
	assert.False(t, ok)
}

func TestStateToAction_ThermostatNilNestedTrait(t *testing.T) {
	d := &device.Device{Id: "tstat1", Details: &device.Device_Thermostat{Thermostat: &device.Thermostat{}}}
	_, err := stateToAction(d)
	assert.Error(t, err)
}

func sensorWithAQI(id string, aqi int32) *device.Device {
	return &device.Device{
		Id: id,
		Details: &device.Device_Sensor{Sensor: &device.Sensor{
			AirQuality: &trait.AirQuality{State: &trait.AirQuality_State{Aqi: &aqi}},
		}},
	}
}

func TestDeviceToAction_SensorAirQuality(t *testing.T) {
	d := sensorWithAQI("sensor1", 42)
	ad, ok := deviceToAction(d)
	assert.True(t, ok)
	assert.Equal(t, "action.devices.types.SENSOR", ad.Type)
	assert.True(t, ad.Traits["action.devices.traits.SensorState"])
}

func TestDeviceToAction_SensorNoAirQuality(t *testing.T) {
	d := &device.Device{Id: "sensor1", Details: &device.Device_Sensor{Sensor: &device.Sensor{}}}
	_, ok := deviceToAction(d)
	assert.False(t, ok)
}

func TestStateToAction_SensorAirQuality(t *testing.T) {
	d := sensorWithAQI("sensor1", 42)
	ds, err := stateToAction(d)
	assert.NoError(t, err)
	data, ok := ds.State["currentSensorStateData"].([]map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "AirQuality", data[0]["name"])
	assert.Equal(t, int32(42), data[0]["rawValue"])
}

func TestBuildCommand_SensorNotExecutable(t *testing.T) {
	d := sensorWithAQI("sensor1", 42)
	_, ok := buildCommand(d, "sensor1", action.Command{OnOff: &action.CommandOnOff{On: true}})
	assert.False(t, ok)
}
