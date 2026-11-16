// Package agent implements googlehome-agentd: the half of the integration
// that runs alongside housed, translating between BridgeService
// (api/device, api/command) and the google-smart-home-action-go library's
// Device/DeviceState/Command types.
//
// Translated so far, in the order they were added: Light (on/off,
// brightness, colour RGB/HSB, colour temperature); AVReceiver and
// Television (on/off, volume, input - Television reuses AVReceiver's
// helpers since both traits are identical, just optional on Television);
// Fan (on/off, speed as a percentage of the device's own range); Thermostat
// (on/off is not translated - house's thermostat.proto has no on_off
// control surface of its own, see SetThermostatMode/OFF instead - setpoint
// and mode); EVCharger (on/off only - wall_power/vehicle_power/
// exterior_conditions/charging_session are all read-only measurements with
// no Google Smart Home trait to report them through); Sensor (read-only AQI
// via SensorState, the one measurement this device kind translates).
//
// Camera (CameraStream, webrtc only, video-only - no audio track in this
// first cut) is the one kind that doesn't fit this package's usual
// deviceToAction/stateToAction/buildCommand shape: GetCameraStream has no
// device state to mutate, so relay.RemoteProvider.Execute intercepts it
// before it ever reaches this agent's generic BridgeService command path
// (see its doc comment) - this agent's own role is
// agent/media_session.go's StartMediaSession handler (pulling the
// device's local WHEP endpoint and forwarding the RTP to googlehomed),
// not buildCommand. deviceToAction/stateToAction still have a Camera
// case (cameraToAction/the Device_Camera case below), just a minimal one -
// see relay/webrtc_session.go's doc comment for the WebRTC/pion side of
// this, and relay/session_manager.go for the session lifecycle. No good
// Google Smart Home type exists for UPS/ConnectedDevice/Clock/
// StandingDesk/Generic/MediaPlayer (MediaPlayer's casting/now-playing
// model doesn't fit a voice-control trait) - those are permanently out of
// scope, not pending work.
//
// Extending to a new device kind means: add a case to the type switches in
// deviceToAction/stateToAction/buildCommand, and a deviceToAction/
// stateToAction/buildCommand helper for it (or reuse an existing one, as
// Television does) - nowhere else.
package agent

import (
	"fmt"
	"math"

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
	switch dd := d.GetDetails().(type) {
	case *device.Device_Light:
		ad, ok = lightToAction(d.GetId(), dd.Light)
	case *device.Device_AvReceiver:
		ad, ok = avLikeDeviceToAction(d.GetId(), "action.devices.types.AUDIO_VIDEO_RECEIVER",
			dd.AvReceiver.GetOnOff(), dd.AvReceiver.GetVolume(), dd.AvReceiver.GetInput())
	case *device.Device_Television:
		ad, ok = avLikeDeviceToAction(d.GetId(), "action.devices.types.TV",
			dd.Television.GetOnOff(), dd.Television.GetVolume(), dd.Television.GetInput())
	case *device.Device_Fan:
		ad, ok = fanToAction(d.GetId(), dd.Fan)
	case *device.Device_Thermostat:
		ad, ok = thermostatToAction(d.GetId(), dd.Thermostat)
	case *device.Device_EvCharger:
		ad, ok = evChargerToAction(d.GetId(), dd.EvCharger)
	case *device.Device_Sensor:
		ad, ok = sensorToAction(d.GetId(), dd.Sensor)
	case *device.Device_Camera:
		ad, ok = cameraToAction(d.GetId(), dd.Camera)
	default:
		return nil, false
	}
	if !ok {
		return nil, false
	}

	applyCommonDeviceInfo(ad, d)
	return ad, true
}

// applyCommonDeviceInfo sets the fields every translated device kind fills in identically -
// display name (falling back to the device ID), manufacturer/model/software version, and
// WillReportState (every kind here reports state via ReportState, so this is unconditional).
func applyCommonDeviceInfo(ad *action.Device, d *device.Device) {
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
}

// stateToAction converts d's current trait state into a Google DeviceState,
// for QUERY responses and state-change reports (ReportState).
func stateToAction(d *device.Device) (action.DeviceState, error) {
	online := d.GetAddress() == nil || d.GetAddress().GetIsReachable()
	unsupported := fmt.Errorf("agent: device %q has no supported trait state", d.GetId())

	switch dd := d.GetDetails().(type) {
	case *device.Device_Light:
		if dd.Light == nil {
			return action.DeviceState{}, unsupported
		}
		return lightStateToAction(online, dd.Light), nil
	case *device.Device_AvReceiver:
		if dd.AvReceiver == nil {
			return action.DeviceState{}, unsupported
		}
		return avLikeStateToAction(online, dd.AvReceiver.GetOnOff(), dd.AvReceiver.GetVolume(), dd.AvReceiver.GetInput()), nil
	case *device.Device_Television:
		if dd.Television == nil {
			return action.DeviceState{}, unsupported
		}
		return avLikeStateToAction(online, dd.Television.GetOnOff(), dd.Television.GetVolume(), dd.Television.GetInput()), nil
	case *device.Device_Fan:
		if dd.Fan == nil {
			return action.DeviceState{}, unsupported
		}
		return fanStateToAction(online, dd.Fan), nil
	case *device.Device_Thermostat:
		if dd.Thermostat == nil || dd.Thermostat.GetThermostat() == nil {
			return action.DeviceState{}, unsupported
		}
		return thermostatStateToAction(online, dd.Thermostat), nil
	case *device.Device_EvCharger:
		if dd.EvCharger == nil {
			return action.DeviceState{}, unsupported
		}
		return evChargerStateToAction(online, dd.EvCharger), nil
	case *device.Device_Sensor:
		ds, ok := sensorStateToAction(online, dd.Sensor)
		if !ok {
			return action.DeviceState{}, unsupported
		}
		return ds, nil
	case *device.Device_Camera:
		if dd.Camera.GetMediaStream() == nil {
			return action.DeviceState{}, unsupported
		}
		// CameraStream is request/response (GetCameraStream, handled
		// entirely by relay.RemoteProvider.Execute and
		// agent/media_session.go - see their doc comments) - there's
		// nothing persistent about a stream session to report here.
		return action.NewDeviceState(online), nil
	default:
		return action.DeviceState{}, unsupported
	}
}

// buildCommand converts one action.Command targeting d (whose kind/colour
// mode decides how an ambiguous field like ColorAbsolute is interpreted -
// see lightBuildCommand's colour case) into the single house command.Command
// it corresponds to, or ok=false if cmd isn't a command this device kind
// supports (the caller reports that device ID as a failure, not a
// best-effort partial application).
func buildCommand(d *device.Device, deviceID string, cmd action.Command) (cc *command.Command, ok bool) {
	switch dd := d.GetDetails().(type) {
	case *device.Device_Light:
		return lightBuildCommand(dd.Light, deviceID, cmd)
	case *device.Device_AvReceiver:
		return avLikeBuildCommand(dd.AvReceiver.GetOnOff(), dd.AvReceiver.GetVolume(), dd.AvReceiver.GetInput(), deviceID, cmd)
	case *device.Device_Television:
		return avLikeBuildCommand(dd.Television.GetOnOff(), dd.Television.GetVolume(), dd.Television.GetInput(), deviceID, cmd)
	case *device.Device_Fan:
		return fanBuildCommand(dd.Fan, deviceID, cmd)
	case *device.Device_Thermostat:
		return thermostatBuildCommand(dd.Thermostat, deviceID, cmd)
	case *device.Device_EvCharger:
		// Sensor falls through to the default case below: it's read-only, no command applies.
		return evChargerBuildCommand(dd.EvCharger, deviceID, cmd)
	default:
		return nil, false
	}
}

// lightToAction builds the action.Device profile for a Light - see the package doc comment for
// which traits are translated.
func lightToAction(id string, light *device.Light) (*action.Device, bool) {
	if light == nil {
		return nil, false
	}

	ad := action.NewLight(id)
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

	return ad, true
}

// lightStateToAction converts light's current trait state into a Google DeviceState.
func lightStateToAction(online bool, light *device.Light) action.DeviceState {
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

	return ds
}

// lightBuildCommand converts one action.Command targeting light into the house command.Command
// it corresponds to, or ok=false if cmd isn't supported for a light (or light itself is nil, or
// lacks the sub-trait cmd targets).
func lightBuildCommand(light *device.Light, deviceID string, cmd action.Command) (*command.Command, bool) {
	if light == nil {
		return nil, false
	}

	cc := &command.Command{DeviceId: deviceID}

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

// avLikeDeviceToAction builds the OnOff+Volume+InputSelector trait set shared by AVReceiver and
// Television - their house proto representations differ only in whether these three sub-traits
// are optional (Television) or not (AVReceiver), and the Go types involved (trait.OnOff,
// trait.Volume, trait.Input) are identical either way. ok is false only if all three are nil -
// a Television with nothing populated isn't translatable, but an AVReceiver always has at least
// OnOff.
//
// This deliberately doesn't call the library's own action.NewSimpleAVReceiver, even for the
// AVReceiver case where it would otherwise fit: that constructor passes canMute into
// AddInputSelectorTrait's "ordered" parameter instead of AddVolumeTrait's "canMute" one (see
// device.go's NewSimpleAVReceiver), which is wrong often enough (any canMute=true receiver gets
// mis-declared as having an ordered input list) that building the trait set by hand here is
// safer than depending on it.
func avLikeDeviceToAction(id, googleType string, onOff *trait.OnOff, volume *trait.Volume, input *trait.Input) (*action.Device, bool) {
	if onOff == nil && volume == nil && input == nil {
		return nil, false
	}

	ad := action.NewDevice(id, googleType)
	if onOff != nil {
		// queryOnly when the bridge marked this on/off state read-only (e.g. a receiver whose
		// power state is sensed but not switchable) - see addVolumeTrait's doc comment for why
		// this can't be done the same way for Volume/Input.
		ad.AddOnOffTrait(false, !onOff.GetAttributes().GetCanControl())
	}
	addVolumeTrait(ad, volume)
	addInputTrait(ad, input)

	return ad, true
}

// addVolumeTrait adds Google's Volume trait to ad if v is populated and controllable. Unlike
// OnOff, this library's AddVolumeTrait has no onlyQuery-equivalent parameter, so a Volume this
// house can't control (bridges/cast/devices.go sets CanControl false for a "fixed"-type Cast
// group, which has no software volume path at all) is left off the SYNC response entirely
// rather than advertised as commandable.
func addVolumeTrait(ad *action.Device, v *trait.Volume) {
	if v == nil || !v.GetAttributes().GetCanControl() {
		return
	}
	ad.AddVolumeTrait(int(v.GetAttributes().GetMaximumLevel()), v.GetAttributes().GetCanMute(), false)
}

// addInputTrait adds Google's InputSelector trait to ad if in is populated and controllable -
// see addVolumeTrait's doc comment (AddInputSelectorTrait has the same onlyQuery gap).
func addInputTrait(ad *action.Device, in *trait.Input) {
	if in == nil || !in.GetAttributes().GetCanControl() {
		return
	}

	var inputs []action.DeviceInput
	for _, i := range in.GetAttributes().GetInputs() {
		name := i.GetName()
		if name == "" {
			name = i.GetId()
		}
		inputs = append(inputs, action.DeviceInput{
			Key:   i.GetId(),
			Names: []action.DeviceInputName{{LanguageCode: "en", Synonyms: []string{name}}},
		})
	}
	ad.AddInputSelectorTrait(inputs, in.GetAttributes().GetIsOrdered())
}

// avLikeStateToAction converts the shared OnOff/Volume/Input sub-traits into a Google
// DeviceState - see avLikeDeviceToAction.
func avLikeStateToAction(online bool, onOff *trait.OnOff, volume *trait.Volume, input *trait.Input) action.DeviceState {
	ds := action.NewDeviceState(online)
	if onOff != nil {
		ds.RecordOnOff(onOff.GetState().GetIsOn())
	}
	if volume != nil {
		ds.RecordVolume(int(volume.GetState().GetLevel()), volume.GetState().GetIsMuted())
	}
	if input != nil {
		ds.RecordInput(input.GetState().GetCurrentInputId())
	}
	return ds
}

// avLikeBuildCommand converts one action.Command into the house command.Command it corresponds
// to, for the shared OnOff/Volume/Input sub-traits - see avLikeDeviceToAction. NextInput and
// PreviousInput aren't supported: house's command.Input only sets an input by ID, it has no
// relative "step to the next/previous one" command.
func avLikeBuildCommand(onOff *trait.OnOff, volume *trait.Volume, input *trait.Input, deviceID string, cmd action.Command) (*command.Command, bool) {
	cc := &command.Command{DeviceId: deviceID}

	switch {
	case cmd.OnOff != nil:
		if onOff == nil || !onOff.GetAttributes().GetCanControl() {
			return nil, false
		}
		cc.Details = &command.Command_OnOff{OnOff: &command.OnOff{On: cmd.OnOff.On}}
	case cmd.SetVolume != nil:
		if volume == nil || !volume.GetAttributes().GetCanControl() {
			return nil, false
		}
		cc.Details = &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: int32(cmd.SetVolume.Level)}}
	case cmd.AdjustVolume != nil:
		if volume == nil || !volume.GetAttributes().GetCanControl() {
			return nil, false
		}
		cc.Details = &command.Command_VolumeRelative{VolumeRelative: &command.VolumeRelative{Delta: int32(cmd.AdjustVolume.Amount)}}
	case cmd.Mute != nil:
		if volume == nil || !volume.GetAttributes().GetCanControl() {
			return nil, false
		}
		cc.Details = &command.Command_Mute{Mute: &command.Mute{IsMuted: cmd.Mute.Mute}}
	case cmd.SetInput != nil:
		if input == nil || !input.GetAttributes().GetCanControl() {
			return nil, false
		}
		cc.Details = &command.Command_Input{Input: &command.Input{InputId: cmd.SetInput.NewInput}}
	default:
		return nil, false
	}

	return cc, true
}

// fanToAction builds the action.Device profile for a Fan: OnOff is always added (house's
// Fan.on_off is not optional; queryOnly if the bridge marked it read-only), and Google's
// FanSpeed trait (as a percentage of the device's own minimum_speed/maximum_speed range - see
// fanSpeedPercent) is added when the Speed trait is populated and controllable. Mode/Timer/
// Toggle/Temperature aren't translated - no Google trait fits a fan's named mode/timer/toggle
// set well enough to be worth it for this pass.
func fanToAction(id string, f *device.Fan) (*action.Device, bool) {
	if f == nil {
		return nil, false
	}

	ad := action.NewDevice(id, "action.devices.types.FAN")
	ad.AddOnOffTrait(false, !f.GetOnOff().GetAttributes().GetCanControl())
	if sp := f.GetSpeed(); sp != nil && sp.GetAttributes().GetCanControl() {
		ad.Traits["action.devices.traits.FanSpeed"] = true
		ad.Attributes["supportsFanSpeedPercent"] = true
		ad.Attributes["reversible"] = false
	}

	return ad, true
}

// fanStateToAction converts f's current trait state into a Google DeviceState.
func fanStateToAction(online bool, f *device.Fan) action.DeviceState {
	ds := action.NewDeviceState(online)
	ds.RecordOnOff(f.GetOnOff().GetState().GetIsOn())
	if sp := f.GetSpeed(); sp != nil {
		ds.State["currentFanSpeedPercent"] = fanSpeedPercent(sp)
	}
	return ds
}

// fanBuildCommand converts one action.Command targeting f into the house command.Command it
// corresponds to, or ok=false if cmd isn't supported for a fan.
func fanBuildCommand(f *device.Fan, deviceID string, cmd action.Command) (*command.Command, bool) {
	if f == nil {
		return nil, false
	}

	cc := &command.Command{DeviceId: deviceID}

	switch {
	case cmd.OnOff != nil:
		if !f.GetOnOff().GetAttributes().GetCanControl() {
			return nil, false
		}
		cc.Details = &command.Command_OnOff{OnOff: &command.OnOff{On: cmd.OnOff.On}}
	case cmd.SetFanSpeed != nil:
		sp := f.GetSpeed()
		if sp == nil || !sp.GetAttributes().GetCanControl() || cmd.SetFanSpeed.FanSpeedPercent == nil {
			// A named fanSpeed (from availableFanSpeeds) is never sent: fanToAction only
			// advertises supportsFanSpeedPercent, never a named-speed list.
			return nil, false
		}
		cc.Details = &command.Command_Speed{Speed: &command.Speed{Value: fanSpeedFromPercent(sp, *cmd.SetFanSpeed.FanSpeedPercent)}}
	default:
		return nil, false
	}

	return cc, true
}

// fanSpeedPercent maps sp's current_speed from its device-defined [minimum_speed,
// maximum_speed] range onto Google's [0, 100] currentFanSpeedPercent scale. A single-speed fan
// (minimum_speed == maximum_speed - e.g. bridges/esphome's SupportedSpeedCount < 1, which
// defaults both to 1) has exactly one speed, which is reported as 100%, not 0%: 0% would tell
// Google a running single-speed fan is off-speed.
func fanSpeedPercent(sp *trait.Speed) int {
	lo, hi := sp.GetAttributes().GetMinimumSpeed(), sp.GetAttributes().GetMaximumSpeed()
	if hi <= lo {
		return 100
	}
	pct := float64(sp.GetState().GetCurrentSpeed()-lo) / float64(hi-lo) * 100.0
	return clampInt(int(math.Round(pct)), 0, 100)
}

// fanSpeedFromPercent is fanSpeedPercent's inverse: it maps a Google fanSpeedPercent command
// value back onto sp's device-defined [minimum_speed, maximum_speed] range. A single-speed fan
// (see fanSpeedPercent) has only one valid value regardless of the requested percent.
func fanSpeedFromPercent(sp *trait.Speed, pct float32) int32 {
	lo, hi := sp.GetAttributes().GetMinimumSpeed(), sp.GetAttributes().GetMaximumSpeed()
	if hi <= lo {
		return lo
	}
	val := lo + int32(math.Round(float64(pct)/100.0*float64(hi-lo)))
	return int32(clampInt(int(val), int(lo), int(hi)))
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// googleThermostatMode maps a house trait.Thermostat_Mode onto one of Google's
// availableThermostatModes/thermostatMode string values. ok is false for MODE_UNSPECIFIED,
// which has no Google equivalent.
func googleThermostatMode(m trait.Thermostat_Mode) (string, bool) {
	switch m {
	case trait.Thermostat_OFF:
		return "off", true
	case trait.Thermostat_HEAT:
		return "heat", true
	case trait.Thermostat_COOL:
		return "cool", true
	case trait.Thermostat_AUTO:
		return "heatcool", true
	default:
		return "", false
	}
}

// houseThermostatMode is googleThermostatMode's inverse, used to decode a
// ThermostatSetMode command (see thermostatBuildCommand).
func houseThermostatMode(s string) (trait.Thermostat_Mode, bool) {
	switch s {
	case "off":
		return trait.Thermostat_OFF, true
	case "heat":
		return trait.Thermostat_HEAT, true
	case "cool":
		return trait.Thermostat_COOL, true
	case "heatcool":
		return trait.Thermostat_AUTO, true
	default:
		return trait.Thermostat_MODE_UNSPECIFIED, false
	}
}

// thermostatToAction builds the action.Device profile for a Thermostat, using Google's
// TemperatureSetting trait. on_off is not translated: house's thermostat.proto has no separate
// on/off control distinct from thermostat.Mode (switching to OFF mode is how a house thermostat
// is turned off - see thermostatBuildCommand's ThermostatSetMode case).
func thermostatToAction(id string, t *device.Thermostat) (*action.Device, bool) {
	if t == nil || t.GetThermostat() == nil {
		return nil, false
	}

	ad := action.NewDevice(id, "action.devices.types.THERMOSTAT")
	ad.Traits["action.devices.traits.TemperatureSetting"] = true

	var modes []string
	for _, m := range t.GetThermostat().GetAttributes().GetSupportedModes() {
		if g, ok := googleThermostatMode(m); ok {
			modes = append(modes, g)
		}
	}
	if len(modes) == 0 {
		modes = []string{"off"}
	}
	ad.Attributes["availableThermostatModes"] = modes

	unit := "C"
	if t.GetThermostat().GetState().GetDisplayUnits() == trait.Thermostat_FAHRENHEIT {
		unit = "F"
	}
	ad.Attributes["thermostatTemperatureUnit"] = unit

	return ad, true
}

// thermostatStateToAction converts t's current trait state into a Google DeviceState.
func thermostatStateToAction(online bool, t *device.Thermostat) action.DeviceState {
	ds := action.NewDeviceState(online)
	s := t.GetThermostat().GetState()

	if g, ok := googleThermostatMode(s.GetTargetMode()); ok {
		ds.State["thermostatMode"] = g
	}
	ds.State["thermostatTemperatureAmbient"] = s.GetCurrentTemperatureCelsius()
	if s != nil && s.CurrentHumidityPercent != nil {
		ds.State["thermostatHumidityAmbient"] = s.GetCurrentHumidityPercent()
	}

	switch s.GetTargetMode() {
	case trait.Thermostat_HEAT:
		ds.State["thermostatTemperatureSetpoint"] = s.GetHeatSetpointCelsius()
	case trait.Thermostat_COOL:
		ds.State["thermostatTemperatureSetpoint"] = s.GetCoolSetpointCelsius()
	case trait.Thermostat_AUTO:
		ds.State["thermostatTemperatureSetpointHigh"] = s.GetCoolSetpointCelsius()
		ds.State["thermostatTemperatureSetpointLow"] = s.GetHeatSetpointCelsius()
	}

	return ds
}

// thermostatBuildCommand converts one action.Command targeting t into the house command.Command
// it corresponds to. ThermostatTemperatureSetpoint is the library's one typed thermostat
// command; mode changes (ThermostatSetMode) have no typed Command field in
// google-smart-home-action-go, so they arrive as cmd.Generic instead and are decoded by hand via
// thermostatModeParam.
func thermostatBuildCommand(t *device.Thermostat, deviceID string, cmd action.Command) (*command.Command, bool) {
	if t == nil || t.GetThermostat() == nil || !t.GetThermostat().GetAttributes().GetCanControl() {
		return nil, false
	}

	cc := &command.Command{DeviceId: deviceID}

	switch {
	case cmd.ThermostatTemperatureSetpoint != nil:
		switch t.GetThermostat().GetState().GetTargetMode() {
		case trait.Thermostat_HEAT, trait.Thermostat_COOL:
			cc.Details = &command.Command_SetTemperature{SetTemperature: &command.SetTemperature{
				SetpointCelsius: cmd.ThermostatTemperatureSetpoint.ThermostatTemperatureSetpointCelcius,
			}}
		default:
			// AUTO needs both bounds (ThermostatTemperatureSetRange, which this library
			// doesn't model as a typed command) and OFF has no setpoint to change - reject
			// rather than guess which bound a single setpoint was meant to move.
			return nil, false
		}
	case cmd.Generic != nil && cmd.Generic.Command == "action.devices.commands.ThermostatSetMode":
		mode, ok := thermostatModeParam(cmd.Generic.Params)
		if !ok {
			return nil, false
		}
		cc.Details = &command.Command_SetThermostatMode{SetThermostatMode: &command.SetThermostatMode{Mode: mode}}
	default:
		return nil, false
	}

	return cc, true
}

// thermostatModeParam decodes a ThermostatSetMode command's generic params - which have no
// static Go type, see thermostatBuildCommand - into a house trait.Thermostat_Mode.
func thermostatModeParam(params map[string]interface{}) (trait.Thermostat_Mode, bool) {
	raw, ok := params["thermostatMode"].(string)
	if !ok {
		return trait.Thermostat_MODE_UNSPECIFIED, false
	}
	return houseThermostatMode(raw)
}

// evChargerToAction builds the action.Device profile for an EVCharger: OnOff is the only
// controllable trait house's model exposes - wall_power, vehicle_power, exterior_conditions and
// charging_session (see api/device/ev_charger.proto) are all read-only measurements with no
// Google Smart Home trait to report them through. Google's CHARGER device type accepts a bare
// OnOff trait. queryOnly when the bridge marked on/off read-only (bridges/tesla-charger sets
// CanControl false: the wall connector's contactor state is sensed from vehicle vitals, not
// something house can command).
func evChargerToAction(id string, e *device.EVCharger) (*action.Device, bool) {
	if e == nil {
		return nil, false
	}
	ad := action.NewDevice(id, "action.devices.types.CHARGER")
	ad.AddOnOffTrait(false, !e.GetOnOff().GetAttributes().GetCanControl())
	return ad, true
}

// evChargerStateToAction converts e's current trait state into a Google DeviceState.
func evChargerStateToAction(online bool, e *device.EVCharger) action.DeviceState {
	ds := action.NewDeviceState(online)
	ds.RecordOnOff(e.GetOnOff().GetState().GetIsOn())
	return ds
}

// evChargerBuildCommand converts an OnOff action.Command into the house command.Command it
// corresponds to - the only command an EVCharger supports, see evChargerToAction.
func evChargerBuildCommand(e *device.EVCharger, deviceID string, cmd action.Command) (*command.Command, bool) {
	if e == nil || cmd.OnOff == nil || !e.GetOnOff().GetAttributes().GetCanControl() {
		return nil, false
	}
	return &command.Command{
		DeviceId: deviceID,
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: cmd.OnOff.On}},
	}, true
}

// sensorToAction builds the action.Device profile for a Sensor's air quality index reading,
// using Google's (read-only) SensorState trait with a single AQI numeric state - the example
// Google's own SensorState trait documentation uses for an air quality monitor. Only
// Sensor.air_quality's AQI value is translated; the sensor's other measurements (battery,
// presence, buttons, the other AirQuality sub-fields, ...) have no Google Smart Home trait
// mapping chosen for them yet - see the package doc comment. ok is false if the device has no
// AirQuality trait at all, or no AQI reading within it.
func sensorToAction(id string, s *device.Sensor) (*action.Device, bool) {
	if s.GetAirQuality().GetState() == nil || s.GetAirQuality().GetState().Aqi == nil {
		return nil, false
	}

	ad := action.NewDevice(id, "action.devices.types.SENSOR")
	ad.Traits["action.devices.traits.SensorState"] = true
	ad.Attributes["sensorStatesSupported"] = []map[string]interface{}{
		{
			"name":                "AirQuality",
			"numericCapabilities": map[string]interface{}{"rawValueUnit": "AQI"},
		},
	}

	return ad, true
}

// sensorStateToAction converts s's AQI reading into a Google DeviceState. ok is false under the
// same condition as sensorToAction.
func sensorStateToAction(online bool, s *device.Sensor) (action.DeviceState, bool) {
	st := s.GetAirQuality().GetState()
	if st == nil || st.Aqi == nil {
		return action.DeviceState{}, false
	}

	ds := action.NewDeviceState(online)
	ds.State["currentSensorStateData"] = []map[string]interface{}{
		{"name": "AirQuality", "rawValue": *st.Aqi},
	}
	return ds, true
}

// cameraToAction returns the Google Smart Home Action profile for a
// Camera device: CameraStream, webrtc only, with a per-session bearer
// token. Unlike every other kind here, GetCameraStream commands never
// reach buildCommand/stateToAction at all - relay.RemoteProvider.Execute
// intercepts them before anything is sent to the agent's generic
// BridgeService command path, since there's no device state to mutate
// (see its doc comment). This agent's own role in a stream session is
// agent/media_session.go's StartMediaSession handler, not translate.go.
// ok is false if c has no media stream to proxy at all.
func cameraToAction(id string, c *device.Camera) (*action.Device, bool) {
	if c.GetMediaStream() == nil {
		return nil, false
	}

	ad := action.NewDevice(id, "action.devices.types.CAMERA")
	ad.Traits["action.devices.traits.CameraStream"] = true
	ad.Attributes["cameraStreamSupportedProtocols"] = []string{"webrtc"}
	ad.Attributes["cameraStreamNeedAuthToken"] = true

	return ad, true
}
