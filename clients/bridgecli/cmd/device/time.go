package device

import (
	"errors"

	"github.com/davecgh/go-spew/spew"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/trait"
	"github.com/spf13/cobra"
)

var (
	displayMode string
	timeZone    string
)

func init() {
	timeCmd.Flags().StringVar(&displayMode, "displayMode", "", "set the time to be in 12 or 24 hour mode")
	timeCmd.Flags().StringVar(&timeZone, "tz", "", "set the time zone to use")
	deviceCmd.AddCommand(timeCmd)
}

var timeCmd = &cobra.Command{
	Use:   "time",
	Short: "Set the time properties of a device",
	RunE: func(cmd *cobra.Command, args []string) error {
		if displayMode == "" && timeZone == "" {
			return errors.New("at least one of displayMode or tz must be set")
		}

		tc := &command.Time{}

		if displayMode == "12" {
			tc.Format = new(trait.Time_Format)
			*tc.Format = trait.Time_TIME_FORMAT_12H
		} else if displayMode == "24" {
			tc.Format = new(trait.Time_Format)
			*tc.Format = trait.Time_TIME_FORMAT_24H
		}

		if len(timeZone) > 0 {
			tc.Timezone = &timeZone
		}

		req := &command.Command{
			DeviceId: id,
			Details:  &command.Command_Time{Time: tc},
		}

		resp, err := client.ExecuteCommand(cmd.Context(), req)
		if err != nil {
			return err
		}

		spew.Dump(resp)

		return nil
	},
}
