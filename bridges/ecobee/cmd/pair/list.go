package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rmrobinson/house/bridges/lib/homekitctrl"
)

func init() {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List accessories already paired in the store",
		RunE:  runList,
	}
	rootCmd.AddCommand(cmd)
}

func runList(cmd *cobra.Command, args []string) error {
	store := homekitctrl.NewFileStore(storePath)

	accessories, err := store.Accessories()
	if err != nil {
		return err
	}
	if len(accessories) == 0 {
		fmt.Println("No accessories paired yet.")
		return nil
	}

	for _, a := range accessories {
		fmt.Printf("%s: pairing id %s, last known address %s:%d\n", a.Name, a.PairingID, a.LastKnownIP, a.LastKnownPort)
	}
	return nil
}
