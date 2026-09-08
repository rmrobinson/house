package homekitctrl

import (
	"crypto/ed25519"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileStoreControllerIdentityRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairing.json")
	store := NewFileStore(path)

	_, err := store.ControllerIdentity()
	require.NoError(t, err)

	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	want := &ControllerIdentity{PairingID: "controller-1", PublicKey: pub, PrivateKey: priv}

	require.NoError(t, store.SaveControllerIdentity(want))

	got, err := store.ControllerIdentity()
	require.NoError(t, err)
	assert.Equal(t, want, got)

	// A second store instance pointed at the same file should see the same data - this is what
	// makes pairing durable across process restarts.
	reopened := NewFileStore(path)
	got, err = reopened.ControllerIdentity()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestFileStoreAccessoryNotFound(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "pairing.json"))

	_, err := store.Accessory("ecobee")
	assert.Error(t, err)
}

func TestFileStoreSaveAccessoryAddsAndUpdates(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "pairing.json"))

	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	rec := &AccessoryRecord{
		Name:          "ecobee",
		PairingID:     "1E:B9:B7:18:41:EA",
		PublicKey:     pub,
		LastKnownIP:   "10.17.15.188",
		LastKnownPort: 38075,
	}
	require.NoError(t, store.SaveAccessory(rec))

	got, err := store.Accessory("ecobee")
	require.NoError(t, err)
	assert.Equal(t, rec, got)

	all, err := store.Accessories()
	require.NoError(t, err)
	assert.Len(t, all, 1)

	// Saving again under the same name updates in place rather than appending a duplicate.
	updated := &AccessoryRecord{Name: "ecobee", PairingID: rec.PairingID, PublicKey: pub, LastKnownIP: "10.17.15.200", LastKnownPort: 38075}
	require.NoError(t, store.SaveAccessory(updated))

	all, err = store.Accessories()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "10.17.15.200", all[0].LastKnownIP)
}
