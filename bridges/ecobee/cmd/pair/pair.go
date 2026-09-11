package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	homekit "github.com/mctofu/homekit/client"
	"github.com/spf13/cobra"

	"github.com/rmrobinson/house/bridges/lib/homekitctrl"
)

var (
	pairAccessoryID string
	pairPIN         string
	pairName        string
	pairTimeout     time.Duration
)

// TODO: generalize this into a bridge-specific admin RPC (accepting an operator-supplied PIN)
// once there's a second bridge that needs the same one-time-pairing flow, instead of a
// per-bridge CLI binary like this one.
func init() {
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Pair with an accessory found via discover, and save the result to the pairing store",
		RunE:  runPair,
	}
	cmd.Flags().StringVar(&pairAccessoryID, "id", "", "accessory ID reported by 'discover'")
	cmd.MarkFlagRequired("id")
	cmd.Flags().StringVar(&pairPIN, "pin", "", "the accessory's HomeKit setup code, including dashes (XXX-XX-XXX)")
	cmd.MarkFlagRequired("pin")
	cmd.Flags().StringVar(&pairName, "name", "ecobee", "alias to save this pairing under")
	// mDNS discovery against this exact ecobee/library combination has been observed needing
	// more than one attempt under a 10s budget (see discover's own --timeout for the same
	// reason) - default a bit higher here since, unlike discover, a timeout here means redoing
	// the whole PIN-entry flow, not just re-running a read-only command.
	cmd.Flags().DurationVar(&pairTimeout, "timeout", 15*time.Second, "how long to wait for the accessory to respond to discovery")
	rootCmd.AddCommand(cmd)
}

func runPair(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	store := homekitctrl.NewFileStore(storePath)

	identity, err := ensureControllerIdentity(store)
	if err != nil {
		return fmt.Errorf("controller identity: %w", err)
	}

	if existing, err := store.Accessory(pairName); err == nil {
		return fmt.Errorf("%q is already paired (accessory id %s) - remove it from %s first to re-pair", pairName, existing.PairingID, storePath)
	}

	device, err := homekit.DeviceByID(ctx, pairAccessoryID, pairTimeout)
	if err != nil {
		return fmt.Errorf("find accessory %s: %w", pairAccessoryID, err)
	}
	if len(device.IPs) == 0 {
		return fmt.Errorf("accessory %s advertised no addresses", pairAccessoryID)
	}

	setupClient := homekit.NewSetupClient(&http.Client{})
	accConn, err := setupClient.Pair(ctx,
		&homekit.AccessoryPairingConfig{
			PIN:      pairPIN,
			DeviceID: pairAccessoryID,
			IPConnectionInfo: homekit.IPConnectionInfo{
				IPAddress: device.IPs[0].String(),
				Port:      device.Port,
			},
			PairingMethod: device.FeatureFlags.PairingMethod(),
		},
		&homekit.ControllerIdentity{
			DeviceID:   identity.PairingID,
			PublicKey:  []byte(identity.PublicKey),
			PrivateKey: []byte(identity.PrivateKey),
		},
	)
	if err != nil {
		return fmt.Errorf("pair: %w", err)
	}

	if err := store.SaveAccessory(&homekitctrl.AccessoryRecord{
		Name:          pairName,
		PairingID:     accConn.DeviceID,
		PublicKey:     ed25519.PublicKey(accConn.PublicKey),
		LastKnownIP:   accConn.IPConnectionInfo.IPAddress,
		LastKnownPort: accConn.IPConnectionInfo.Port,
	}); err != nil {
		return fmt.Errorf("save pairing: %w", err)
	}

	fmt.Printf("Paired %q successfully. Saved to %s.\n", pairName, storePath)
	return nil
}

// ensureControllerIdentity returns this controller's persisted identity, generating and saving
// one if this is the first pairing ever done against storePath.
func ensureControllerIdentity(store homekitctrl.Store) (*homekitctrl.ControllerIdentity, error) {
	existing, err := store.ControllerIdentity()
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate controller key: %w", err)
	}
	identity := &homekitctrl.ControllerIdentity{
		PairingID:  uuid.New().String(),
		PublicKey:  pub,
		PrivateKey: priv,
	}
	if err := store.SaveControllerIdentity(identity); err != nil {
		return nil, err
	}

	fmt.Printf("Generated new controller identity %s\n", identity.PairingID)
	return identity, nil
}
