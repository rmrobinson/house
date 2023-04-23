package bridge

import (
	api2 "github.com/rmrobinson/house/api"
	"github.com/spf13/cobra"
)

var (
	id string

	client api2.BridgeServiceClient

	bridgeCmd = &cobra.Command{
		Use:   "bridge",
		Short: "Run bridge commands against a bridge",
		Long:  ``,
	}
)

func Init(cmd *cobra.Command) {
	bridgeCmd.PersistentFlags().StringVar(&id, "bridgeID", "", "bridge ID to manage")
	bridgeCmd.MarkPersistentFlagRequired("bridgeID")

	cmd.AddCommand(bridgeCmd)
}

func Setup(c api2.BridgeServiceClient) {
	client = c
}
