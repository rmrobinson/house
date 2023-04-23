package cmd

import (
	"fmt"
	"os"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/clients/bridgecli/cmd/bridge"
	"github.com/rmrobinson/house/clients/bridgecli/cmd/device"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	bridgeAddr   string
	bridgeConn   *grpc.ClientConn
	bridgeClient api2.BridgeServiceClient

	rootCmd = &cobra.Command{
		Use:   "bridge",
		Short: "Allows for control of the specified bridge",
		Long:  ``,
	}
)

// Execute is the entry point into the command hierarchy
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initClient)
	cobra.OnFinalize(closeClient)

	rootCmd.PersistentFlags().StringVar(&bridgeAddr, "addr", "", "bridge API address to connect to")
	rootCmd.MarkPersistentFlagRequired("addr")

	device.Init(rootCmd)
	bridge.Init(rootCmd)
}

func initClient() {
	if len(bridgeAddr) < 1 {
		return
	}

	var opts []grpc.DialOption
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	conn, err := grpc.Dial(bridgeAddr, opts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	bridgeConn = conn
	bridgeClient = api2.NewBridgeServiceClient(bridgeConn)

	device.Setup(bridgeClient)
	bridge.Setup(bridgeClient)
}

func closeClient() {
	if bridgeConn != nil {
		bridgeConn.Close()
	}
}
