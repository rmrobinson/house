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

// TestTimeoutClosesConnectionRatherThanRiskingStaleReplyMisdelivery is the regression test for a
// bug found during review: do() previously just gave up and returned an error on timeout, leaving
// the connection open - but HAP has no pipelining and no request ID, so if the accessory was just
// slow (not actually dead) and eventually did reply, that late reply had nothing to distinguish it
// from the response to whatever unrelated call happened to be pending next on the same Controller.
// do() now closes the connection on timeout instead, so a stale reply can never be misdelivered:
// the Controller becomes fully unusable, forcing a real reconnect.
func TestTimeoutClosesConnectionRatherThanRiskingStaleReplyMisdelivery(t *testing.T) {
	controller := testControllerIdentity(t)
	fa := newFakeAccessory(t, controller.PairingID, controller.PublicKey)
	fa.hangReads = true // the accessory is slow, not dead - it would eventually reply if asked to

	c, err := Connect(context.Background(), fa.addr(), controller, fa.accessoryRecord())
	require.NoError(t, err)
	defer c.Close()

	disconnectCh := make(chan error, 1)
	c.evMu.Lock()
	c.onDisconnect = func(err error) { disconnectCh <- err }
	c.evMu.Unlock()

	readCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = c.ReadCharacteristics(readCtx, []CharID{{1, 19}})
	assert.Error(t, err)

	// The timeout should have torn the connection down (rather than leaving it open for a
	// subsequent call to risk receiving this abandoned request's eventual/late reply) -
	// evidenced by onDisconnect firing.
	select {
	case <-disconnectCh:
	case <-time.After(3 * time.Second):
		t.Fatal("expected the timed-out request to close the connection and fire onDisconnect")
	}

	// A subsequent call on the same (now-dead) Controller must fail immediately, not hang or
	// silently succeed with misdelivered data.
	_, err = c.ReadCharacteristics(context.Background(), []CharID{{1, 19}})
	assert.Error(t, err)
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

// TestConnectClearsPairVerifyDeadline is the regression test for a real bug caught during live
// testing: pair-verify's postTLV8 calls (verify.go) set a deadline on the raw conn via
// withDeadline but never clear it. Previously that was masked because do() also called
// withDeadline on every single request, continuously refreshing/replacing it - but now that do()
// waits on a channel instead of reading the conn directly, nothing was clearing that leftover
// deadline, so it would silently expire and permanently fail every future Read on runReader's
// goroutine, well after pair-verify itself succeeded and had nothing further to do with it.
func TestConnectClearsPairVerifyDeadline(t *testing.T) {
	controller := testControllerIdentity(t)
	fa := newFakeAccessory(t, controller.PairingID, controller.PublicKey)
	fa.setCharacteristic(CharID{1, 19}, 22.8)

	// A short deadline on the ctx passed to Connect (rather than mutating any package state) is
	// enough to make withDeadline's pair-verify-phase deadline short too, per its own "prefer
	// ctx.Deadline() over defaultOperationTimeout" logic - keeping this test fast and isolated
	// from any other test running in this package.
	connectCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c, err := Connect(connectCtx, fa.addr(), controller, fa.accessoryRecord())
	require.NoError(t, err)
	defer c.Close()

	// Long enough past that shrunk connect-time deadline that an uncleared deadline on the
	// underlying conn would already have expired.
	time.Sleep(300 * time.Millisecond)

	values, err := c.ReadCharacteristics(context.Background(), []CharID{{1, 19}})
	require.NoError(t, err)
	require.Len(t, values, 1)
}

func TestSubscribeThenPushDeliversEvent(t *testing.T) {
	controller := testControllerIdentity(t)
	fa := newFakeAccessory(t, controller.PairingID, controller.PublicKey)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := Connect(ctx, fa.addr(), controller, fa.accessoryRecord())
	require.NoError(t, err)
	defer c.Close()

	eventsCh := make(chan []CharacteristicValue, 1)
	require.NoError(t, c.Subscribe(ctx, []CharID{{1, 19}}, func(values []CharacteristicValue) {
		eventsCh <- values
	}, nil))
	assert.True(t, fa.isSubscribed(CharID{1, 19}))

	fa.pushEvent(CharID{1, 19}, 21.5)

	select {
	case values := <-eventsCh:
		require.Len(t, values, 1)
		assert.Equal(t, CharID{1, 19}, CharID{values[0].AccessoryID, values[0].CharacteristicID})
		assert.JSONEq(t, "21.5", string(values[0].Value))
	case <-time.After(3 * time.Second):
		t.Fatal("event was not delivered in time")
	}
}

func TestEventDuringPendingRequestDoesNotCorruptResponse(t *testing.T) {
	controller := testControllerIdentity(t)
	fa := newFakeAccessory(t, controller.PairingID, controller.PublicKey)
	fa.setCharacteristic(CharID{1, 19}, 22.8)
	fa.replyDelay = 300 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := Connect(ctx, fa.addr(), controller, fa.accessoryRecord())
	require.NoError(t, err)
	defer c.Close()

	eventsCh := make(chan []CharacteristicValue, 1)
	c.evMu.Lock()
	c.onEvent = func(values []CharacteristicValue) { eventsCh <- values }
	c.evMu.Unlock()

	readDone := make(chan struct{})
	var values []CharacteristicValue
	var readErr error
	go func() {
		values, readErr = c.ReadCharacteristics(ctx, []CharID{{1, 19}})
		close(readDone)
	}()

	time.Sleep(50 * time.Millisecond) // let the read actually start and be pending on the fake's replyDelay
	fa.pushEvent(CharID{1, 76}, 2)

	select {
	case ev := <-eventsCh:
		require.Len(t, ev, 1)
		assert.Equal(t, uint64(76), ev[0].CharacteristicID)
	case <-time.After(3 * time.Second):
		t.Fatal("event was not delivered while a request was pending")
	}

	select {
	case <-readDone:
	case <-time.After(3 * time.Second):
		t.Fatal("pending read never completed")
	}
	require.NoError(t, readErr)
	require.Len(t, values, 1)
	assert.JSONEq(t, "22.8", string(values[0].Value))
}

func TestCloseUnblocksBackgroundReaderAndPendingSubscriber(t *testing.T) {
	controller := testControllerIdentity(t)
	fa := newFakeAccessory(t, controller.PairingID, controller.PublicKey)
	fa.hangReads = true

	connectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(connectCtx, fa.addr(), controller, fa.accessoryRecord())
	require.NoError(t, err)

	disconnectCh := make(chan error, 1)
	c.evMu.Lock()
	c.onDisconnect = func(err error) { disconnectCh <- err }
	c.evMu.Unlock()

	errCh := make(chan error, 1)
	go func() {
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

	select {
	case err := <-disconnectCh:
		assert.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Close() did not fire onDisconnect in time")
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
