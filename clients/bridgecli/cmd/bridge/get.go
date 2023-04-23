package bridge

import (
	"github.com/davecgh/go-spew/spew"
	api2 "github.com/rmrobinson/house/api"
	"github.com/spf13/cobra"
)

func init() {
	bridgeCmd.AddCommand(getCmd)
}

var getCmd = &cobra.Command{
	Use:   "get",
	Short: "Get the bridge",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &api2.GetBridgeRequest{
			Id: id,
		}

		resp, err := client.GetBridge(cmd.Context(), req)
		if err != nil {
			return err
		}

		spew.Dump(resp)

		return nil
	},
}
