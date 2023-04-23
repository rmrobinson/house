package device

import (
	"github.com/davecgh/go-spew/spew"
	"github.com/rmrobinson/house/api/command"
	"github.com/spf13/cobra"
)

var (
	brightness int32
)

func init() {
	brightnessCmd.Flags().Int32Var(&brightness, "brightness", 0, "the brightness to set this device to")
	brightnessCmd.MarkFlagRequired("brightness")
	deviceCmd.AddCommand(brightnessCmd)
}

var brightnessCmd = &cobra.Command{
	Use:   "brightness",
	Short: "Set the brightness of a device",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &command.Command{
			DeviceId: id,
			Details:  &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: brightness}},
		}

		resp, err := client.ExecuteCommand(cmd.Context(), req)
		if err != nil {
			return err
		}

		spew.Dump(resp)

		return nil
	},
}
