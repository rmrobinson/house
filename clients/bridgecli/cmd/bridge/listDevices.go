package bridge

import (
	"github.com/davecgh/go-spew/spew"
	"github.com/spf13/cobra"

	api2 "github.com/rmrobinson/house/api"
)

func init() {
	bridgeCmd.AddCommand(listDevicesCmd)
}

var listDevicesCmd = &cobra.Command{
	Use:   "listDevices",
	Short: "Get the devices on the bridge",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &api2.ListDevicesRequest{}

		resp, err := client.ListDevices(cmd.Context(), req)
		if err != nil {
			return err
		}

		spew.Dump(resp)

		return nil
	},
}
