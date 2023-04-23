package main

import (
	"context"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
	"go.uber.org/zap"
)

// ClockBridge acts as the handler for Bridge requests for this Clock.
type ClockBridge struct {
	logger *zap.Logger

	c *Clock

	d *device.Device
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (cb *ClockBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	// The clock supports the Brightness and OnOff commands.
	if cmd.GetOnOff() != nil {
		cb.c.SetOnOff(cmd.GetOnOff().On)

		cb.d.GetClock().OnOff.State.IsOn = cmd.GetOnOff().On
	} else if cmd.GetBrightnessAbsolute() != nil {
		cb.c.SetBrightness(int(cmd.GetBrightnessAbsolute().BrightnessPercent))

		cb.d.GetClock().Brightness.State.Level = cmd.GetBrightnessAbsolute().BrightnessPercent
	} else if cmd.GetBrightnessRelative() != nil {
		cb.c.ChangeBrightness(int(cmd.GetBrightnessRelative().ChangePercent))

		cb.d.GetClock().Brightness.State.Level += cmd.GetBrightnessRelative().ChangePercent
	} else if cmd.GetTime() != nil {
		if cmd.GetTime().GetFormat() == trait.Time_TIME_FORMAT_12H {
			cb.c.SetTimeMode(TwelveHour)

			cb.d.GetClock().Time.State.TimeFormat = trait.Time_TIME_FORMAT_12H
		} else if cmd.GetTime().GetFormat() == trait.Time_TIME_FORMAT_24H {
			cb.c.SetTimeMode(TwentyFourHour)

			cb.d.GetClock().Time.State.TimeFormat = trait.Time_TIME_FORMAT_24H
		}

		if cmd.GetTime().GetTimezone() != "" {
			if err := cb.c.SetTimeZone(cmd.GetTime().GetTimezone()); err != nil {
				cb.logger.Error("invalid timezone specified", zap.String("timezone", cmd.GetTime().GetTimezone()))
				return nil, bridge.ErrInvalidTimezone
			}

			cb.d.GetClock().Time.State.Timezone = cmd.GetTime().GetTimezone()
		}
	} else {
		cb.logger.Error("received unsupported command - shouldn't happen")
		return nil, bridge.ErrUnsupportedCommand
	}

	return cb.d, nil
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it does nothing
// since there isn't 'remote' state which needs to be refreshed.
func (cb *ClockBridge) Refresh(ctx context.Context) error {
	return nil
}
