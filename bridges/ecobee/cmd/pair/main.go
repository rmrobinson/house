// Command pair is a one-time, interactive tool for pairing this bridge's controller with a
// HomeKit accessory (an ecobee thermostat) and saving the result into a JSON store the ecobee
// bridge reads at startup.
//
// This is a separate binary rather than something exposed over the bridge's own gRPC surface
// because pairing requires reading a PIN off the physical accessory - there's no way to do that
// through a remote API call. A future version could add a bridge-specific admin RPC that accepts
// an operator-supplied PIN instead of requiring shell access to wherever the bridge runs, but
// that's not implemented here - see the TODO on the pair command below.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var storePath string

var rootCmd = &cobra.Command{
	Use:   "pair",
	Short: "Pair this bridge's controller with a HomeKit accessory",
}

func main() {
	rootCmd.PersistentFlags().StringVar(&storePath, "store", "ecobee-pairing.json", "path to the pairing store JSON file (the ecobee bridge's config points at the same path)")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
