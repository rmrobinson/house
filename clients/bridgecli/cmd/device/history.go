package device

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
)

var (
	historyWithin     time.Duration
	historyMaxSamples int32
)

func init() {
	historyCmd.Flags().DurationVar(&historyWithin, "within", 24*time.Hour, "fetch samples recorded within this long before now")
	historyCmd.Flags().Int32Var(&historyMaxSamples, "maxSamples", 0, "cap the number of samples returned (0 for no cap); the oldest are kept")
	deviceCmd.AddCommand(historyCmd)
}

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "Fetch the samples a device recorded itself",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &api2.GetDeviceHistoryRequest{
			DeviceId:   id,
			Since:      timestamppb.New(time.Now().Add(-historyWithin)),
			MaxSamples: historyMaxSamples,
		}

		stream, err := client.GetDeviceHistory(cmd.Context(), req)
		if err != nil {
			return err
		}

		count := 0
		for {
			sample, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}

			fmt.Printf("%s %s\n", sample.GetTime().AsTime().Format(time.RFC3339), prototext.MarshalOptions{}.Format(sample.GetSnapshot()))
			count++
		}
		fmt.Printf("%d samples\n", count)

		return nil
	},
}
