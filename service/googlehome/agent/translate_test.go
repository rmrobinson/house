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
