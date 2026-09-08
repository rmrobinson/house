package homekitctrl

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testControllerIdentity(t *testing.T) *ControllerIdentity {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return &ControllerIdentity{PairingID: "11111111-1111-1111-1111-111111111111", PublicKey: pub, PrivateKey: priv}
}

func TestConnectAndReadCharacteristics(t *testing.T) {
	controller := testControllerIdentity(t)
	fa := newFakeAccessory(t, controller.PairingID, controller.PublicKey)
	fa.setCharacteristic(CharID{1, 19}, 22.8)
	fa.setCharacteristic(CharID{1, 27}, "Main Floor")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := Connect(ctx, fa.addr(), controller, fa.accessoryRecord())
	require.NoError(t, err)
	defer c.Close()

	values, err := c.ReadCharacteristics(ctx, []CharID{{1, 19}, {1, 27}})
	require.NoError(t, err)
	require.Len(t, values, 2)

	byID := map[CharID]CharacteristicValue{}
	for _, v := range values {
		byID[CharID{v.AccessoryID, v.CharacteristicID}] = v
	}
	assert.JSONEq(t, "22.8", string(byID[CharID{1, 19}].Value))
	assert.JSONEq(t, `"Main Floor"`, string(byID[CharID{1, 27}].Value))
}

func TestWriteCharacteristics(t *testing.T) {
	controller := testControllerIdentity(t)
	fa := newFakeAccessory(t, controller.PairingID, controller.PublicKey)
	fa.setCharacteristic(CharID{1, 20}, 21.0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := Connect(ctx, fa.addr(), controller, fa.accessoryRecord())
	require.NoError(t, err)
	defer c.Close()

	require.NoError(t, c.WriteCharacteristics(ctx, []CharacteristicWrite{{AccessoryID: 1, CharacteristicID: 20, Value: 23.5}}))

	values, err := c.ReadCharacteristics(ctx, []CharID{{1, 20}})
	require.NoError(t, err)
	require.Len(t, values, 1)
	assert.JSONEq(t, "23.5", string(values[0].Value))
}

func TestWriteCharacteristicsReportsPerItemFailure(t *testing.T) {
	controller := testControllerIdentity(t)
	fa := newFakeAccessory(t, controller.PairingID, controller.PublicKey)
	failing := CharID{1, 40}
	fa.failWriteOf = &failing

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := Connect(ctx, fa.addr(), controller, fa.accessoryRecord())
	require.NoError(t, err)
	defer c.Close()

	err = c.WriteCharacteristics(ctx, []CharacteristicWrite{{AccessoryID: 1, CharacteristicID: 40, Value: 2}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "1.40")
}

func TestReadCharacteristicsTimesOutRatherThanHangingForever(t *testing.T) {
	controller := testControllerIdentity(t)
	fa := newFakeAccessory(t, controller.PairingID, controller.PublicKey)
	fa.hangReads = true

	connectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(connectCtx, fa.addr(), controller, fa.accessoryRecord())
	require.NoError(t, err)
	defer c.Close()

	readCtx, readCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer readCancel()

	start := time.Now()
	_, err = c.ReadCharacteristics(readCtx, []CharID{{1, 19}})
	elapsed := time.Since(start)

	assert.Error(t, err)
	assert.Less(t, elapsed, 3*time.Second, "read should have returned via ctx's deadline, not hung")
}

func TestCloseUnblocksAPendingRead(t *testing.T) {
	controller := testControllerIdentity(t)
	fa := newFakeAccessory(t, controller.PairingID, controller.PublicKey)
	fa.hangReads = true

	connectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(connectCtx, fa.addr(), controller, fa.accessoryRecord())
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() {
		// Deliberately no deadline on this ctx - relies entirely on Close() to unblock it, not
		// a timeout, to isolate that Close() itself (not just withDeadline's fallback timeout)
		// can recover a hung call. If Close() were still guarded by the same mutex do() holds,
		// this would block for defaultOperationTimeout (30s) instead of returning promptly.
		_, err := c.ReadCharacteristics(context.Background(), []CharID{{1, 19}})
		errCh <- err
	}()

	time.Sleep(100 * time.Millisecond) // let the read actually start and block
	require.NoError(t, c.Close())

	select {
	case err := <-errCh:
		assert.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Close() did not unblock the pending read in time")
	}
}

func TestConnectFailsIfAccessoryLTPKDoesNotMatch(t *testing.T) {
	controller := testControllerIdentity(t)
	fa := newFakeAccessory(t, controller.PairingID, controller.PublicKey)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Present a record with the right PairingID but the wrong public key, simulating a
	// mismatched/spoofed accessory - pair-verify must reject this rather than silently trusting it.
	wrongPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	badRecord := &AccessoryRecord{Name: "fake", PairingID: fa.pairingID, PublicKey: wrongPub}

	_, err = Connect(ctx, fa.addr(), controller, badRecord)
	assert.Error(t, err)
}
