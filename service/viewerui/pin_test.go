package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// clockedGate is a gate for "1234" whose clock the test controls.
func clockedGate(t *testing.T) (*pinGate, *time.Time) {
	t.Helper()
	g := testPINGate(t, "1234")
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	return g, &now
}

func TestPINGate_NotConfigured(t *testing.T) {
	var nilGate *pinGate
	assert.ErrorIs(t, nilGate.check("1234"), errPINNotConfigured)

	g, err := newPINGate("")
	require.NoError(t, err)
	assert.ErrorIs(t, g.check("1234"), errPINNotConfigured)
	assert.ErrorIs(t, g.check(""), errPINNotConfigured, "an empty PIN must not match an empty hash")
}

func TestPINGate_RejectsAHashThatIsNotBcrypt(t *testing.T) {
	// A plain-text PIN in the hash field is the likely misconfiguration, and must not be accepted.
	_, err := newPINGate("1234")
	assert.Error(t, err)
}

func TestPINGate_RightAndWrong(t *testing.T) {
	g, _ := clockedGate(t)
	assert.ErrorIs(t, g.check("0000"), errPINWrong)
	assert.ErrorIs(t, g.check(""), errPINWrong)
	assert.NoError(t, g.check("1234"))
}

func TestPINGate_LocksWithDoublingBackoffAndRecovers(t *testing.T) {
	g, now := clockedGate(t)

	for i := 0; i < pinFreeAttempts; i++ {
		assert.ErrorIs(t, g.check("0000"), errPINWrong, "attempt %d", i+1)
	}
	var locked pinLockedError
	require.ErrorAs(t, g.check("1234"), &locked, "the right PIN is refused while locked")
	assert.Equal(t, pinBaseLockout, locked.retryAfter)

	*now = now.Add(pinBaseLockout)
	assert.ErrorIs(t, g.check("0000"), errPINWrong) // lock over; this miss doubles the next lock
	require.ErrorAs(t, g.check("1234"), &locked)
	assert.Equal(t, 2*pinBaseLockout, locked.retryAfter)

	*now = now.Add(2 * pinBaseLockout)
	assert.NoError(t, g.check("1234"), "right PIN after the lock clears it")
	// ...and resets the count: it takes a full pinFreeAttempts misses to lock again.
	for i := 0; i < pinFreeAttempts-1; i++ {
		assert.ErrorIs(t, g.check("0000"), errPINWrong)
	}
	assert.NoError(t, g.check("1234"))
}

func TestPINGate_LockoutIsCapped(t *testing.T) {
	g, now := clockedGate(t)
	for i := 0; i < 200; i++ { // far past where the shift would overflow
		_ = g.check("0000")
		*now = g.lockedUntil // jump to the end of whatever lock was set
	}
	_ = g.check("0000")
	var locked pinLockedError
	require.ErrorAs(t, g.check("1234"), &locked)
	assert.Equal(t, pinMaxLockout, locked.retryAfter)
}

func TestPINGate_StoresNoPlainPIN(t *testing.T) {
	g := testPINGate(t, "1234")
	assert.NotContains(t, string(g.hash), "1234")
	assert.NoError(t, bcrypt.CompareHashAndPassword(g.hash, []byte("1234")))
}
