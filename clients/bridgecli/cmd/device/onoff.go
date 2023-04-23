package device

import (
	"github.com/davecgh/go-spew/spew"
	"github.com/rmrobinson/house/api/command"
	"github.com/spf13/cobra"
)

var (
	isOn bool
)

func init() {
	onOffCmd.Flags().BoolVar(&isOn, "on", false, "whether to turn it on or off")
	onOffCmd.MarkFlagRequired("on")
	deviceCmd.AddCommand(onOffCmd)
}

var onOffCmd = &cobra.Command{
	Use:   "onoff",
	Short: "Turn a device on or off",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &command.Command{
			DeviceId: id,
			Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: isOn}},
		}

		resp, err := client.ExecuteCommand(cmd.Context(), req)
		if err != nil {
			return err
		}

		spew.Dump(resp)

		return nil
	},
}
