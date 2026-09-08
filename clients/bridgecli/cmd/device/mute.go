package device

import (
	"github.com/davecgh/go-spew/spew"
	"github.com/rmrobinson/house/api/command"
	"github.com/spf13/cobra"
)

var (
	isMuted bool
)

func init() {
	muteCmd.Flags().BoolVar(&isMuted, "muted", false, "whether to mute or unmute the device")
	muteCmd.MarkFlagRequired("muted")
	deviceCmd.AddCommand(muteCmd)
}

var muteCmd = &cobra.Command{
	Use:   "mute",
	Short: "Mute or unmute a device",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &command.Command{
			DeviceId: id,
			Details:  &command.Command_Mute{Mute: &command.Mute{IsMuted: isMuted}},
		}

		resp, err := client.ExecuteCommand(cmd.Context(), req)
		if err != nil {
			return err
		}

		spew.Dump(resp)

		return nil
	},
}
