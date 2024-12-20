package bridge

import (
	"fmt"
	"io"

	"github.com/davecgh/go-spew/spew"
	"github.com/spf13/cobra"

	api2 "github.com/rmrobinson/house/api"
)

func init() {
	bridgeCmd.AddCommand(monitorCmd)
}

var monitorCmd = &cobra.Command{
	Use:   "monitor",
	Short: "Watch for updates from the bridge",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &api2.StreamUpdatesRequest{}

		stream, err := client.StreamUpdates(cmd.Context(), req)
		if err != nil {
			return err
		}

		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				fmt.Printf("remote side closed the connection\n")
				return nil
			} else if msg == nil {
				fmt.Printf("nothing to receive, exiting\n")
				return nil
			}

			spew.Dump(msg)
		}

		return nil
	},
}
