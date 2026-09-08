package device

import (
	"fmt"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/trait"
	"github.com/spf13/cobra"
)

var thermostatMode string

func init() {
	thermostatModeCmd.Flags().StringVar(&thermostatMode, "mode", "", "off, heat, cool, or auto")
	thermostatModeCmd.MarkFlagRequired("mode")
	deviceCmd.AddCommand(thermostatModeCmd)
}

var thermostatModeCmd = &cobra.Command{
	Use:   "thermostatMode",
	Short: "Set a thermostat's HVAC mode",
	RunE: func(cmd *cobra.Command, args []string) error {
		mode, err := parseThermostatMode(thermostatMode)
		if err != nil {
			return err
		}

		req := &command.Command{
			DeviceId: id,
			Details:  &command.Command_SetThermostatMode{SetThermostatMode: &command.SetThermostatMode{Mode: mode}},
		}

		resp, err := client.ExecuteCommand(cmd.Context(), req)
		if err != nil {
			return err
		}

		spew.Dump(resp)

		return nil
	},
}

// parseThermostatMode accepts the enum's values case-insensitively (e.g. "heat", not just
// "HEAT") since that's friendlier to type on a command line than shouting.
func parseThermostatMode(s string) (trait.Thermostat_Mode, error) {
	v, ok := trait.Thermostat_Mode_value[strings.ToUpper(s)]
	if !ok {
		return trait.Thermostat_MODE_UNSPECIFIED, fmt.Errorf("unknown mode %q (want off, heat, cool, or auto)", s)
	}
	return trait.Thermostat_Mode(v), nil
}
