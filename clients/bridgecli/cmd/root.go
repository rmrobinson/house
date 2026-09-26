package cmd

import (
	"fmt"
	"os"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/clients/bridgecli/cmd/bridge"
	"github.com/rmrobinson/house/clients/bridgecli/cmd/device"
	"github.com/rmrobinson/house/grpcutil"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

var (
	bridgeAddr   string
	tlsCertFile  string
	tlsKeyFile   string
	tlsCAFile    string
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

	// Optional mutual TLS - all three are required together, or all left
	// blank to dial plaintext gRPC (the default), matching how a bridge's
	// own bridge.tls.* config works.
	rootCmd.PersistentFlags().StringVar(&tlsCertFile, "tls-cert", "", "client certificate file for mutual TLS")
	rootCmd.PersistentFlags().StringVar(&tlsKeyFile, "tls-key", "", "client key file for mutual TLS")
	rootCmd.PersistentFlags().StringVar(&tlsCAFile, "tls-ca", "", "CA file trusted to verify the bridge's certificate")

	device.Init(rootCmd)
	bridge.Init(rootCmd)
}

func initClient() {
	if len(bridgeAddr) < 1 {
		return
	}

	var conn *grpc.ClientConn
	var err error
	if len(tlsCertFile) > 0 {
		conn, err = grpcutil.DialTLS(bridgeAddr, grpcutil.ClientTLSConfig{
			CertFile: tlsCertFile,
			KeyFile:  tlsKeyFile,
			CAFile:   tlsCAFile,
		})
	} else {
		conn, err = grpcutil.DialInsecure(bridgeAddr)
	}
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
