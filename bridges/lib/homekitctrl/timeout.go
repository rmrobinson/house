package homekitctrl

import (
	"context"
	"net"
	"time"
)

// defaultOperationTimeout bounds a single request/response exchange when ctx carries no deadline
// of its own. ecobee's HAP server is documented to hang or drop connections outright rather than
// erroring promptly, so every wire operation in this package needs a bound - relying solely on
// callers to always supply a context.WithTimeout would make that bound optional instead of a
// guarantee.
const defaultOperationTimeout = 30 * time.Second

// withDeadline runs fn with conn's deadline set from ctx (or defaultOperationTimeout if ctx has
// none), and arranges for conn's deadline to be forced to "now" if ctx is cancelled before fn
// returns - the standard way to make a net.Conn operation (which has no context-aware API of its
// own) responsive to context cancellation.
func withDeadline(ctx context.Context, conn net.Conn, fn func() error) error {
	deadline := time.Now().Add(defaultOperationTimeout)
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.SetDeadline(time.Now())
		case <-done:
		}
	}()

	return fn()
}
