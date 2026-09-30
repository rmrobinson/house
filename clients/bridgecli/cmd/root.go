package cmd

import (
	"fmt"
	"os"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/clients/bridgecli/cmd/bridge"
	"github.com/rmrobinson/house/clients/bridgecli/cmd/device"
	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

var (
	bridgeAddr    string
	tlsCertFile   string
	tlsKeyFile    string
	tlsCAFile     string
	tlsServerName string
	bridgeConn    *grpc.ClientConn
	bridgeClient  api2.BridgeServiceClient

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
	// Optional even when TLS is on: every bridge's cert is issued for
	// "<name>.<host>.house.internal" (see house-config's cert-agent/renew.sh),
	// not whatever --addr actually is (an IP, or a different hostname) - so
	// the default (grpc deriving the expected name from --addr) only works
	// when --addr already happens to be that same name. Leave blank in that
	// case; set explicitly whenever --addr is an IP or anything else.
	rootCmd.PersistentFlags().StringVar(&tlsServerName, "tls-server-name", "", "hostname to verify the bridge's certificate against, if different from --addr")

	device.Init(rootCmd)
	bridge.Init(rootCmd)
}

func initClient() {
	if len(bridgeAddr) < 1 {
		return
	}

	set := 0
	for _, f := range []string{tlsCertFile, tlsKeyFile, tlsCAFile} {
		if len(f) > 0 {
			set++
		}
	}

	var tlsCfg *grpcutil.ClientTLSConfig
	switch set {
	case 0:
		// plaintext gRPC - the default
	case 3:
		tlsCfg = &grpcutil.ClientTLSConfig{CertFile: tlsCertFile, KeyFile: tlsKeyFile, CAFile: tlsCAFile, ServerName: tlsServerName}
	default:
		fmt.Fprintln(os.Stderr, "--tls-cert, --tls-key, and --tls-ca are required together - only some of them were set")
		os.Exit(1)
	}

	conn, err := grpcutil.Dial(bridgeAddr, tlsCfg)
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
