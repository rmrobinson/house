package main

import (
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// pinFreeAttempts is how many wrong PINs in a row are allowed before the gate starts locking.
	pinFreeAttempts = 5
	// pinBaseLockout is the first lock; it doubles with each further wrong PIN, up to pinMaxLockout.
	pinBaseLockout = 30 * time.Second
	pinMaxLockout  = 15 * time.Minute
	// pinMaxDoublings is the most the lock is ever doubled. 30s << 6 already exceeds pinMaxLockout,
	// so clamping here is purely what keeps the shift from overflowing time.Duration.
	pinMaxDoublings = 6
	// pinMaxLen bounds a submitted PIN. It is far above any real PIN and far below bcrypt's 72-byte
	// input limit (older x/crypto silently truncates past it); anything longer is refused before it
	// reaches bcrypt, the lock or the mutex.
	pinMaxLen = 32
)

// The gate's errors are gRPC statuses (see AGENTS.md), so houseview.Message gives the plain text
// shown inline and houseview.HTTPStatus a sensible code if the row can't be re-rendered.
var (
	errPINNotConfigured = status.Error(codes.FailedPrecondition, "no door PIN is configured")
	errPINWrong         = status.Error(codes.PermissionDenied, "incorrect PIN")
)

func pinLocked(retryAfter time.Duration) error {
	return status.Errorf(codes.ResourceExhausted, "too many incorrect PINs; try again in %s", retryAfter.Round(time.Second))
}

// pinGate guards actions behind one shared PIN, kept only as a bcrypt hash. A short numeric PIN
// is trivially brute-forced if guesses are free, so after pinFreeAttempts consecutive wrong
// PINs it locks with a doubling backoff.
//
// The lock is global rather than per client: viewerui sits behind a reverse proxy, so every
// request arrives from the proxy's address, and X-Forwarded-For is client-controlled unless the
// proxy is trusted. The cost is that someone who can reach the viewer can lock the household
// out of the gated actions for a while - an acceptable trade for a door, and the ungated
// actions (CLOSE, lights, ...) keep working. There is no reset short of waiting or restarting.
type pinGate struct {
	hash []byte
	now  func() time.Time

	mu          sync.Mutex
	failures    int
	lockedUntil time.Time
}

// newPINGate returns a gate for hash, a bcrypt hash of the PIN. An empty hash gives a gate that
// refuses everything (errPINNotConfigured), so a deployment that forgot the key fails closed.
func newPINGate(hash string) (*pinGate, error) {
	g := &pinGate{now: time.Now}
	if hash == "" {
		return g, nil
	}
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		return nil, fmt.Errorf("door_pin_hash is not a bcrypt hash: %w", err)
	}
	g.hash = []byte(hash)
	return g, nil
}

// check returns nil if pin matches. A nil gate is treated as unconfigured.
func (g *pinGate) check(pin string) error {
	if g == nil || len(g.hash) == 0 {
		return errPINNotConfigured
	}
	if len(pin) > pinMaxLen {
		// Not counted against the lock: it can't be a real PIN, and costs nothing to refuse.
		return errPINWrong
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// Held across the bcrypt compare on purpose: it serializes attempts, so concurrent guesses
	// can't race past the lockout.
	if wait := g.lockedUntil.Sub(g.now()); wait > 0 {
		return pinLocked(wait)
	}
	if bcrypt.CompareHashAndPassword(g.hash, []byte(pin)) != nil {
		g.failures++
		if g.failures >= pinFreeAttempts {
			doublings := min(g.failures-pinFreeAttempts, pinMaxDoublings)
			g.lockedUntil = g.now().Add(min(pinBaseLockout<<doublings, pinMaxLockout))
		}
		return errPINWrong
	}
	g.failures = 0
	g.lockedUntil = time.Time{}
	return nil
}
