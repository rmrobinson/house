package device

import (
	"github.com/davecgh/go-spew/spew"
	"github.com/rmrobinson/house/api/command"
	"github.com/spf13/cobra"
)

var comfortProfileName string

func init() {
	comfortProfileCmd.Flags().StringVar(&comfortProfileName, "profile", "", "the comfort profile to switch to (e.g. Home, Away, Sleep - must be one of the thermostat's own configured profiles)")
	comfortProfileCmd.MarkFlagRequired("profile")
	deviceCmd.AddCommand(comfortProfileCmd)
}

var comfortProfileCmd = &cobra.Command{
	Use:   "comfortProfile",
	Short: "Switch a thermostat's active comfort profile (e.g. Home/Away/Sleep)",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &command.Command{
			DeviceId: id,
			Details:  &command.Command_SetComfortProfile{SetComfortProfile: &command.SetComfortProfile{ProfileName: comfortProfileName}},
		}

		resp, err := client.ExecuteCommand(cmd.Context(), req)
		if err != nil {
			return err
		}

		spew.Dump(resp)

		return nil
	},
}
