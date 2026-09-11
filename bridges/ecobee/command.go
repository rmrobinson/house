package main

import (
	"fmt"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/bridges/lib/homekitctrl"
	"github.com/rmrobinson/house/service/bridge"
)

func modeToHVACState(m trait.Thermostat_Mode) (int, error) {
	switch m {
	case trait.Thermostat_OFF:
		return 0, nil
	case trait.Thermostat_HEAT:
		return 1, nil
	case trait.Thermostat_COOL:
		return 2, nil
	case trait.Thermostat_AUTO:
		return 3, nil
	default:
		return 0, fmt.Errorf("unsupported thermostat mode %v", m)
	}
}

func validateSetpoint(celsius float32) error {
	if celsius < minSetpointCelsius || celsius > maxSetpointCelsius {
		return fmt.Errorf("setpoint %.1f°C outside supported range [%.1f, %.1f]", celsius, minSetpointCelsius, maxSetpointCelsius)
	}
	return nil
}

// thermostatCommandWrites translates cmd into the characteristic writes needed to apply it, and
// in the same switch/case mutates optimistic's thermostat state to reflect the expected result.
// Doing both together, rather than as two independently-maintained switches over the same oneof,
// is deliberate: a second switch can silently drift out of sync with this one (a new command type
// added to only one of them compiles fine and fails silently), whereas forgetting half of a single
// case here doesn't compile-and-hide, it's just missing from the one place it needs to be.
//
// optimistic's TargetMode is read to decide SetTemperature's routing (HEAT vs COOL) and is
// expected to already reflect a live read if the caller wants that decision made on fresher data
// than the last poll - see ProcessCommand's handling of Command_SetTemperature.
//
// command.SetVentilation and command.SetHumidity are deliberately not handled here: this ecobee
// doesn't expose ERV/HRV control over HAP at all (see README), and this house has no humidifier
// attached, so a TargetRelativeHumidity write would be accepted by the accessory but do nothing -
// better to surface both as an explicit "unsupported" than let either look like a working
// control.
func thermostatCommandWrites(cmd *command.Command, optimistic *device.Device) ([]homekitctrl.CharacteristicWrite, error) {
	state := optimistic.GetThermostat().GetThermostat().GetState()

	switch details := cmd.Details.(type) {
	case *command.Command_SetThermostatMode:
		mode := details.SetThermostatMode.Mode
		v, err := modeToHVACState(mode)
		if err != nil {
			return nil, err
		}
		state.TargetMode = mode
		return []homekitctrl.CharacteristicWrite{
			{AccessoryID: thermostatAID, CharacteristicID: charTargetHeatingCoolingState, Value: v},
		}, nil

	case *command.Command_SetTemperature:
		celsius := details.SetTemperature.SetpointCelsius
		if err := validateSetpoint(celsius); err != nil {
			return nil, err
		}
		switch state.TargetMode {
		case trait.Thermostat_HEAT:
			state.HeatSetpointCelsius = ptr(celsius)
			// optimistic may be a clone of a device last polled in a different mode (e.g. COOL),
			// which would leave its CoolSetpointCelsius set - clear it so the result matches what
			// a fresh read would produce (HEAT/COOL populate exactly one of the pair, never both).
			state.CoolSetpointCelsius = nil
		case trait.Thermostat_COOL:
			state.CoolSetpointCelsius = ptr(celsius)
			state.HeatSetpointCelsius = nil
		default:
			return nil, fmt.Errorf("SetTemperature requires HEAT or COOL mode, thermostat is in %v - use SetHeatCoolSetpoints for AUTO", state.TargetMode)
		}
		return []homekitctrl.CharacteristicWrite{
			{AccessoryID: thermostatAID, CharacteristicID: charTargetTemperature, Value: celsius},
		}, nil

	case *command.Command_SetHeatCoolSetpoints:
		heat := details.SetHeatCoolSetpoints.HeatSetpointCelsius
		cool := details.SetHeatCoolSetpoints.CoolSetpointCelsius
		if err := validateSetpoint(heat); err != nil {
			return nil, err
		}
		if err := validateSetpoint(cool); err != nil {
			return nil, err
		}
		state.HeatSetpointCelsius = ptr(heat)
		state.CoolSetpointCelsius = ptr(cool)
		return []homekitctrl.CharacteristicWrite{
			{AccessoryID: thermostatAID, CharacteristicID: charHeatingThreshold, Value: heat},
			{AccessoryID: thermostatAID, CharacteristicID: charCoolingThreshold, Value: cool},
		}, nil

	case *command.Command_SetComfortProfile:
		name := details.SetComfortProfile.ProfileName
		index, ok := comfortProfileIndex(name)
		if !ok {
			return nil, fmt.Errorf("unknown comfort profile %q, must be one of %v", name, comfortProfileNames)
		}
		state.ActiveComfortProfile = ptr(name)
		return []homekitctrl.CharacteristicWrite{
			{AccessoryID: thermostatAID, CharacteristicID: charSetComfortProfile, Value: index},
		}, nil

	default:
		return nil, bridge.ErrUnsupportedCommand
	}
}
