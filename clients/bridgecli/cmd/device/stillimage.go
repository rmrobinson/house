package device

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/trait"
)

var (
	stillOutput    string
	stillMaxHeight int32
	stillSource    string
	stillEventID   string
)

func init() {
	stillImageCmd.Flags().StringVarP(&stillOutput, "output", "o", "", "file to write the picture to")
	stillImageCmd.Flags().Int32Var(&stillMaxHeight, "maxHeight", 0, "scale the picture down to at most this height (0 for native)")
	stillImageCmd.Flags().StringVar(&stillSource, "source", "", "preferred source: 'event' or 'latest' (default: the device's choice)")
	stillImageCmd.Flags().StringVar(&stillEventID, "eventID", "", "fetch the picture of this detection event")
	stillImageCmd.MarkFlagRequired("output")
	deviceCmd.AddCommand(stillImageCmd)
}

var stillImageCmd = &cobra.Command{
	Use:   "still-image",
	Short: "Fetch a still picture from a device",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &api2.GetStillImageRequest{DeviceId: id, MaxHeight: stillMaxHeight, EventId: stillEventID}
		switch stillSource {
		case "":
		case "event":
			req.PreferredSource = trait.StillImage_EVENT_SNAPSHOT
		case "latest":
			req.PreferredSource = trait.StillImage_LATEST_FRAME
		default:
			return fmt.Errorf("unknown --source %q, want 'event' or 'latest'", stillSource)
		}

		resp, err := client.GetStillImage(cmd.Context(), req)
		if err != nil {
			return err
		}
		if err := os.WriteFile(stillOutput, resp.GetData(), 0o644); err != nil {
			return err
		}

		fmt.Printf("wrote %d bytes of %s to %s (source %s, event %q, captured %s)\n",
			len(resp.GetData()), resp.GetContentType(), stillOutput, resp.GetSource(), resp.GetEventId(),
			resp.GetCapturedAt().AsTime().Format("2006-01-02T15:04:05Z07:00"))
		return nil
	},
}
