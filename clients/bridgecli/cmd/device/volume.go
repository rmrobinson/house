package device

import (
	"github.com/davecgh/go-spew/spew"
	"github.com/rmrobinson/house/api/command"
	"github.com/spf13/cobra"
)

var (
	volumeDelta int32
)

func init() {
	volumeCmd.Flags().Int32Var(&volumeDelta, "delta", 0, "the number of steps to change the volume by (negative to decrease)")
	volumeCmd.MarkFlagRequired("delta")
	deviceCmd.AddCommand(volumeCmd)
}

var volumeCmd = &cobra.Command{
	Use:   "volume",
	Short: "Adjust a device's volume by a relative number of steps",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &command.Command{
			DeviceId: id,
			Details:  &command.Command_VolumeRelative{VolumeRelative: &command.VolumeRelative{Delta: volumeDelta}},
		}

		resp, err := client.ExecuteCommand(cmd.Context(), req)
		if err != nil {
			return err
		}

		spew.Dump(resp)

		return nil
	},
}
