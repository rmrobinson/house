package device

import (
	"github.com/davecgh/go-spew/spew"
	"github.com/rmrobinson/house/api/command"
	"github.com/spf13/cobra"
)

var (
	heatSetpointCelsius float32
	coolSetpointCelsius float32
)

func init() {
	heatCoolSetpointsCmd.Flags().Float32Var(&heatSetpointCelsius, "heatCelsius", 0, "the heat setpoint, in celsius")
	heatCoolSetpointsCmd.MarkFlagRequired("heatCelsius")
	heatCoolSetpointsCmd.Flags().Float32Var(&coolSetpointCelsius, "coolCelsius", 0, "the cool setpoint, in celsius")
	heatCoolSetpointsCmd.MarkFlagRequired("coolCelsius")
	deviceCmd.AddCommand(heatCoolSetpointsCmd)
}

var heatCoolSetpointsCmd = &cobra.Command{
	Use:   "heatCoolSetpoints",
	Short: "Set a thermostat's dual (low/high) setpoints, for AUTO mode",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &command.Command{
			DeviceId: id,
			Details: &command.Command_SetHeatCoolSetpoints{SetHeatCoolSetpoints: &command.SetHeatCoolSetpoints{
				HeatSetpointCelsius: heatSetpointCelsius,
				CoolSetpointCelsius: coolSetpointCelsius,
			}},
		}

		resp, err := client.ExecuteCommand(cmd.Context(), req)
		if err != nil {
			return err
		}

		spew.Dump(resp)

		return nil
	},
}
