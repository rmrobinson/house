package device

import (
	"github.com/davecgh/go-spew/spew"
	"github.com/rmrobinson/house/api/command"
	"github.com/spf13/cobra"
)

var temperatureSetpointCelsius float32

func init() {
	temperatureCmd.Flags().Float32Var(&temperatureSetpointCelsius, "celsius", 0, "the setpoint to set, in celsius")
	temperatureCmd.MarkFlagRequired("celsius")
	deviceCmd.AddCommand(temperatureCmd)
}

var temperatureCmd = &cobra.Command{
	Use:   "temperature",
	Short: "Set a thermostat's single setpoint (valid in HEAT or COOL mode; use heatCoolSetpoints for AUTO)",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &command.Command{
			DeviceId: id,
			Details:  &command.Command_SetTemperature{SetTemperature: &command.SetTemperature{SetpointCelsius: temperatureSetpointCelsius}},
		}

		resp, err := client.ExecuteCommand(cmd.Context(), req)
		if err != nil {
			return err
		}

		spew.Dump(resp)

		return nil
	},
}
