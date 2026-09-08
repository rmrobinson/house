package device

import (
	"github.com/davecgh/go-spew/spew"
	"github.com/rmrobinson/house/api/command"
	"github.com/spf13/cobra"
)

var (
	launchAppID string
)

func init() {
	appLaunchCmd.Flags().StringVar(&launchAppID, "appID", "", "the platform-specific ID of the application to launch")
	appLaunchCmd.MarkFlagRequired("appID")
	appCmd.AddCommand(appLaunchCmd)
	deviceCmd.AddCommand(appCmd)
}

var appCmd = &cobra.Command{
	Use:   "app",
	Short: "Run app commands against a device",
	Long:  ``,
}

var appLaunchCmd = &cobra.Command{
	Use:   "launch",
	Short: "Launch an application on a device",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &command.Command{
			DeviceId: id,
			Details:  &command.Command_AppLaunch{AppLaunch: &command.AppLaunch{AppId: launchAppID}},
		}

		resp, err := client.ExecuteCommand(cmd.Context(), req)
		if err != nil {
			return err
		}

		spew.Dump(resp)

		return nil
	},
}
