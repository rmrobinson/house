package main

import (
	"context"
	"fmt"
	"time"

	homekit "github.com/mctofu/homekit/client"
	"github.com/spf13/cobra"
)

var discoverTimeout time.Duration

func init() {
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "List HomeKit accessories visible via mDNS",
		RunE:  runDiscover,
	}
	cmd.Flags().DurationVar(&discoverTimeout, "timeout", 10*time.Second, "how long to wait for responses")
	rootCmd.AddCommand(cmd)
}

func runDiscover(cmd *cobra.Command, args []string) error {
	found := 0
	err := homekit.Discover(cmd.Context(), func(_ context.Context, d *homekit.AccessoryDevice) {
		found++
		fmt.Printf("Name:   %s\n", d.Name)
		fmt.Printf("Model:  %s\n", d.Model)
		fmt.Printf("ID:     %s\n", d.ID)
		fmt.Printf("IPs:    %v\n", d.IPs)
		fmt.Printf("Port:   %d\n", d.Port)
		fmt.Printf("Status: %s\n\n", d.StatusFlags)
	}, discoverTimeout)
	if err != nil {
		return err
	}

	fmt.Printf("Found %d accessories\n", found)
	return nil
}
