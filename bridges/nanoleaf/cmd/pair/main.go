// Command pair prints a new API key for a Nanoleaf Light Panels controller, for pasting into
// nanoleaf.example.yaml's matching device entry.
//
// Unlike bridges/ecobee/cmd/pair, there's no persistent pairing store: a Nanoleaf API key is a
// single opaque string (not a HomeKit keypair/identity), handled the same way bridges/esphome's
// noise_psk is - pasted directly into config.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	nanoleaf "github.com/rmrobinson/nanoleaf-go"
)

var (
	pairHost    string
	pairPort    int
	pairTimeout time.Duration
	pairPoll    time.Duration
)

var rootCmd = &cobra.Command{
	Use:   "pair",
	Short: "Pair with a Nanoleaf Light Panels controller and print the resulting API key",
	Long: `Hold the panel's power button for 5-7 seconds until its LEDs flash to open its ~30s
pairing window, then run this command (or run it first and press the button while it's polling -
it keeps retrying until --timeout). The panel rejects the request with 401 until the button has
been pressed, which this command treats as "not yet" rather than a fatal error.`,
	RunE: runPair,
}

func main() {
	rootCmd.Flags().StringVar(&pairHost, "host", "", "the IP or hostname of the panel")
	rootCmd.MarkFlagRequired("host")
	rootCmd.Flags().IntVar(&pairPort, "port", 16021, "the control port of the panel")
	rootCmd.Flags().DurationVar(&pairTimeout, "timeout", 30*time.Second, "how long to keep polling for the pairing button press")
	rootCmd.Flags().DurationVar(&pairPoll, "poll", 2*time.Second, "how often to retry while waiting for the button press")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runPair(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), pairTimeout)
	defer cancel()

	c := nanoleaf.NewClient(&http.Client{}, pairHost, pairPort, "")

	fmt.Println("waiting for the panel's pairing button to be pressed (hold the power button 5-7s until the LEDs flash)...")

	ticker := time.NewTicker(pairPoll)
	defer ticker.Stop()

	for {
		key, err := c.CreateAPIKey(ctx)
		if err == nil {
			fmt.Printf("paired - API key: %s\n", key)
			return nil
		}
		if err != nanoleaf.ErrUnauthorized && err != nanoleaf.ErrForbidden {
			return fmt.Errorf("unable to create API key: %w", err)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for the pairing button to be pressed")
		case <-ticker.C:
		}
	}
}
