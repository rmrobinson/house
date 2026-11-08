package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// clockedGate is a gate for "1234" whose clock the test controls.
func clockedGate(t *testing.T) (*pinGate, *time.Time) {
	t.Helper()
	g := testPINGate(t, "1234")
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	return g, &now
}

// lockRemaining is how long the gate will still refuse attempts.
func lockRemaining(g *pinGate) time.Duration { return g.lockedUntil.Sub(g.now()) }

func TestPINGate_NotConfigured(t *testing.T) {
	var nilGate *pinGate
	assert.Equal(t, codes.FailedPrecondition, status.Code(nilGate.check("1234")))

	g, err := newPINGate("")
	require.NoError(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(g.check("1234")))
	assert.Equal(t, codes.FailedPrecondition, status.Code(g.check("")), "an empty PIN must not match an empty hash")
}

func TestPINGate_RejectsAHashThatIsNotBcrypt(t *testing.T) {
	// A plain-text PIN in the hash field is the likely misconfiguration, and must not be accepted.
	_, err := newPINGate("1234")
	assert.Error(t, err)
}

func TestPINGate_RightAndWrong(t *testing.T) {
	g, _ := clockedGate(t)
	assert.Equal(t, codes.PermissionDenied, status.Code(g.check("0000")))
	assert.Equal(t, codes.PermissionDenied, status.Code(g.check("")))
	assert.NoError(t, g.check("1234"))
}

func TestPINGate_LocksWithDoublingBackoffAndRecovers(t *testing.T) {
	g, now := clockedGate(t)

	for i := 0; i < pinFreeAttempts; i++ {
		assert.Equal(t, codes.PermissionDenied, status.Code(g.check("0000")), "attempt %d", i+1)
	}
	err := g.check("1234")
	assert.Equal(t, codes.ResourceExhausted, status.Code(err), "the right PIN is refused while locked")
	assert.Contains(t, status.Convert(err).Message(), "too many incorrect PINs")
	assert.Equal(t, pinBaseLockout, lockRemaining(g))

	*now = now.Add(pinBaseLockout)
	assert.Equal(t, codes.PermissionDenied, status.Code(g.check("0000"))) // lock over; this miss doubles the next lock
	assert.Equal(t, 2*pinBaseLockout, lockRemaining(g))

	*now = now.Add(2 * pinBaseLockout)
	assert.NoError(t, g.check("1234"), "right PIN after the lock clears it")
	// ...and resets the count: it takes a full pinFreeAttempts misses to lock again.
	for i := 0; i < pinFreeAttempts-1; i++ {
		assert.Equal(t, codes.PermissionDenied, status.Code(g.check("0000")))
	}
	assert.NoError(t, g.check("1234"))
}

// The lock must never shrink, never exceed the cap, and never wrap, however many misses pile up -
// the doubling is a shift on a Duration, which overflows quietly if left unbounded.
func TestPINGate_LockoutIsMonotonicAndCapped(t *testing.T) {
	g, now := clockedGate(t)
	var prev time.Duration
	for i := 1; i <= 500; i++ {
		_ = g.check("0000")
		got := lockRemaining(g)
		if i >= pinFreeAttempts {
			require.GreaterOrEqual(t, got, pinBaseLockout, "miss %d", i)
			require.LessOrEqual(t, got, pinMaxLockout, "miss %d", i)
			require.GreaterOrEqual(t, got, prev, "miss %d: lock shrank", i)
			prev = got
		}
		*now = g.lockedUntil // jump to the end of whatever lock was set
	}
	assert.Equal(t, pinMaxLockout, prev)
}

func TestPINGate_OverlongPINRefusedWithoutCountingAgainstTheLock(t *testing.T) {
	g, _ := clockedGate(t)
	long := strings.Repeat("1", pinMaxLen+1)
	for i := 0; i < 3*pinFreeAttempts; i++ {
		assert.Equal(t, codes.PermissionDenied, status.Code(g.check(long)))
	}
	assert.Zero(t, g.failures, "an impossible PIN is refused outright; it must not lock the household out")
	assert.NoError(t, g.check("1234"))
}

func TestPINGate_StoresNoPlainPIN(t *testing.T) {
	g := testPINGate(t, "1234")
	assert.NotContains(t, string(g.hash), "1234")
	assert.NoError(t, bcrypt.CompareHashAndPassword(g.hash, []byte("1234")))
}
