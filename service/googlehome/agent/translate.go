// Package agent implements googlehome-agentd: the half of the integration
// that runs alongside housed, translating between BridgeService
// (api/device, api/command) and the google-smart-home-action-go library's
// Device/DeviceState/Command types.
//
// Only api/device/light.proto's Light is translated today - that's the
// device kind people actually ask Google Assistant to control ("turn on
// the kitchen lights", "dim the bedroom to 20%"). Extending to another
// device kind (Fan, Thermostat, ...) means adding a case to deviceToAction/
// stateToAction/buildCommand and nowhere else - see those functions' doc
// comments for the shape a new kind needs to follow.
package agent

import (
	"fmt"

	action "github.com/rmrobinson/google-smart-home-action-go"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
)

// deviceToAction converts d into the Google Smart Home Action device profile
// to return from SYNC, or ok=false if d's kind isn't supported yet (see the
// package doc comment) - the caller should skip it rather than fail the
// whole SYNC response, the same convention proxy.go's bridgeToGoogleDevice
// used in the nerves precedent this is modeled on.
func deviceToAction(d *device.Device) (ad *action.Device, ok bool) {
	light := d.GetLight()
	if light == nil {
		return nil, false
	}

	ad = action.NewLight(d.GetId())
	if light.GetBrightness() != nil {
		// onlyCommand is always false: BridgeService's cached Device state
		// always answers QUERY, there's no write-only brightness here.
		ad.AddBrightnessTrait(false)
	}
	if c := light.GetColour(); c != nil {
		switch c.GetAttributes().GetMode() {
		case trait.Colour_Attributes_MODE_RGB:
			ad.AddColourTrait(action.RGB, false)
		case trait.Colour_Attributes_MODE_HSB:
			ad.AddColourTrait(action.HSV, false)
		}
		if r := c.GetAttributes().GetColourTemperatureRange(); r != nil {
			ad.AddColourTemperatureTrait(int(r.GetMinK()), int(r.GetMaxK()), false)
		}
	}

	ad.Name.Name = d.GetConfig().GetName()
	if ad.Name.Name == "" {
		ad.Name.Name = d.GetId()
	}
	ad.DeviceInfo.Manufacturer = d.GetManufacturer()
	if d.GetModelName() != "" {
		ad.DeviceInfo.Model = d.GetModelName()
	} else {
		ad.DeviceInfo.Model = d.GetModelId()
	}
	ad.DeviceInfo.SwVersion = d.GetSoftwareVersion()
	ad.WillReportState = true

	return ad, true
}

// stateToAction converts d's current trait state into a Google DeviceState,
// for QUERY responses and state-change reports (ReportState).
func stateToAction(d *device.Device) (action.DeviceState, error) {
	light := d.GetLight()
	if light == nil {
		return action.DeviceState{}, fmt.Errorf("agent: device %q has no supported trait state", d.GetId())
	}

	online := d.GetAddress() == nil || d.GetAddress().GetIsReachable()
	ds := action.NewDeviceState(online)
	ds.RecordOnOff(light.GetOnOff().GetState().GetIsOn())

	if b := light.GetBrightness(); b != nil {
		ds.RecordBrightness(int(b.GetState().GetLevel()))
	}
	if c := light.GetColour(); c != nil {
		switch c.GetAttributes().GetMode() {
		case trait.Colour_Attributes_MODE_RGB:
			rgb := c.GetState().GetRgb()
			spectrum := (int(rgb.GetRed())&0xff)<<16 | (int(rgb.GetGreen())&0xff)<<8 | (int(rgb.GetBlue()) & 0xff)
			ds.RecordColorRGB(spectrum)
		case trait.Colour_Attributes_MODE_HSB:
			hsb := c.GetState().GetHsb()
			ds.RecordColorHSV(float64(hsb.GetHue()), float64(hsb.GetSaturation())/100.0, float64(hsb.GetBrightness())/100.0)
		}
		if c.GetAttributes().GetColourTemperatureRange() != nil && c.GetState().GetColourTemperatureK() > 0 {
			ds.RecordColorTemperature(int(c.GetState().GetColourTemperatureK()))
		}
	}

	return ds, nil
}

// buildCommand converts one action.Command targeting d (whose kind/colour
// mode decides how an ambiguous field like ColorAbsolute is interpreted -
// see the colour case below) into the single house command.Command it
// corresponds to, or ok=false if cmd isn't a command this device kind
// supports (the caller reports that device ID as a failure, not a
// best-effort partial application).
func buildCommand(d *device.Device, deviceID string, cmd action.Command) (cc *command.Command, ok bool) {
	light := d.GetLight()
	if light == nil {
		return nil, false
	}

	cc = &command.Command{DeviceId: deviceID}

	switch {
	case cmd.OnOff != nil:
		cc.Details = &command.Command_OnOff{OnOff: &command.OnOff{On: cmd.OnOff.On}}
	case cmd.BrightnessAbsolute != nil:
		if light.GetBrightness() == nil {
			return nil, false
		}
		cc.Details = &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{
			BrightnessPercent: int32(cmd.BrightnessAbsolute.Brightness),
		}}
	case cmd.BrightnessRelative != nil:
		if light.GetBrightness() == nil {
			return nil, false
		}
		// RelativeWeight (an opaque, device-defined unit - see Google's
		// Brightness trait docs) has no house equivalent; house's
		// BrightnessRelative is percent-only, so only RelativePercent is
		// honoured.
		cc.Details = &command.Command_BrightnessRelative{BrightnessRelative: &command.BrightnessRelative{
			ChangePercent: int32(cmd.BrightnessRelative.RelativePercent),
		}}
	case cmd.ColorAbsolute != nil:
		colour := light.GetColour()
		if colour == nil {
			return nil, false
		}
		col := cmd.ColorAbsolute.Color
		switch {
		case col.Temperature > 0 && colour.GetAttributes().GetColourTemperatureRange() != nil:
			cc.Details = &command.Command_Colour{Colour: &command.Colour{
				Value: &command.Colour_ColourTemperatureK{ColourTemperatureK: int32(col.Temperature)},
			}}
		case colour.GetAttributes().GetMode() == trait.Colour_Attributes_MODE_RGB:
			cc.Details = &command.Command_Colour{Colour: &command.Colour{
				Value: &command.Colour_Rgb{Rgb: &command.Colour_RGB{
					Red:   (int32(col.RGB) >> 16) & 0xff,
					Green: (int32(col.RGB) >> 8) & 0xff,
					Blue:  int32(col.RGB) & 0xff,
				}},
			}}
		case colour.GetAttributes().GetMode() == trait.Colour_Attributes_MODE_HSB:
			cc.Details = &command.Command_Colour{Colour: &command.Colour{
				Value: &command.Colour_Hsb{Hsb: &command.Colour_HSB{
					Hue:        int32(col.HSV.Hue),
					Saturation: int32(col.HSV.Saturation * 100.0),
					Brightness: int32(col.HSV.Value * 100.0),
				}},
			}}
		default:
			return nil, false
		}
	default:
		return nil, false
	}

	return cc, true
}
