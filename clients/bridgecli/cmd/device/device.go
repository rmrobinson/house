package device

import (
	api2 "github.com/rmrobinson/house/api"
	"github.com/spf13/cobra"
)

var (
	id string

	client api2.BridgeServiceClient

	deviceCmd = &cobra.Command{
		Use:   "device",
		Short: "Run device commands against a bridge",
		Long:  ``,
	}
)

func Init(cmd *cobra.Command) {
	deviceCmd.PersistentFlags().StringVar(&id, "deviceID", "", "device ID to manage")
	deviceCmd.MarkPersistentFlagRequired("id")

	cmd.AddCommand(deviceCmd)
}

func Setup(c api2.BridgeServiceClient) {
	client = c
}
