package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/rmrobinson/house/bridges/lib/homekitctrl"
)

const discoverTimeout = 10 * time.Second

// subscribeTimeout bounds the Subscribe call ensureConnected fires off after a fresh connect.
// Independent of any caller-supplied ctx deliberately: Subscribe is best-effort (a failure just
// degrades to poll-only, see ensureConnected), so it shouldn't compete with the caller's own
// ReadCharacteristics/WriteCharacteristics for a share of whatever deadline they set - see
// ensureConnected's use of this via its own context.Background()-derived context.
const subscribeTimeout = 10 * time.Second

// hapController is the subset of *homekitctrl.Controller's API that ecobeeConn/EcobeeBridge
// depend on. Abstracted so tests can exercise the reconnect and command-routing logic in this
// package against a fake, without a real network listener - the wire protocol itself (pair-verify,
// framing, timeouts, event demux) is already covered by homekitctrl's own tests against a fake
// accessory, so there's no value in re-proving that here too.
type hapController interface {
	ReadCharacteristics(ctx context.Context, ids []homekitctrl.CharID) ([]homekitctrl.CharacteristicValue, error)
	WriteCharacteristics(ctx context.Context, writes []homekitctrl.CharacteristicWrite) error
	Subscribe(ctx context.Context, ids []homekitctrl.CharID, onEvent func([]homekitctrl.CharacteristicValue), onDisconnect func(error)) error
	Close() error
}

// ecobeeConn manages a single Controller connection to the paired ecobee, transparently
// reconnecting whenever an operation reports the connection is no longer good. Every reconnect
// re-runs mDNS discovery rather than trusting a cached address, since ecobee's HAP server is
// documented to drop connections and change IPs (DHCP) more often than most HAP accessories -
// see the design notes in README.
type ecobeeConn struct {
	logger        *zap.Logger
	store         homekitctrl.Store
	accessoryName string

	// discover/connect default to the real homekitctrl package calls (set in newEcobeeConn);
	// tests override them with fakes to exercise ensureConnected/invalidate's decisions without
	// a real network.
	discover func(ctx context.Context, pairingID string, timeout time.Duration) (*homekitctrl.DiscoveredAccessory, error)
	connect  func(ctx context.Context, host string, identity *homekitctrl.ControllerIdentity, accessory *homekitctrl.AccessoryRecord) (hapController, error)

	// watchedIDs/onEvent/onDisconnect are set once via configureEvents, after both this
	// ecobeeConn and the EcobeeBridge that owns it exist (configureEvents is separate from the
	// constructor purely to avoid a "closure over eb before eb exists" ordering problem in
	// NewEcobeeBridge). onEvent is nil until then, which ensureConnected treats as "no event
	// support configured" - it just skips Subscribe entirely rather than erroring.
	watchedIDs   func() []homekitctrl.CharID
	onEvent      func([]homekitctrl.CharacteristicValue)
	onDisconnect func(error)

	mu   sync.Mutex
	ctrl hapController
}

func newEcobeeConn(logger *zap.Logger, store homekitctrl.Store, accessoryName string) *ecobeeConn {
	return &ecobeeConn{
		logger:        logger,
		store:         store,
		accessoryName: accessoryName,
		discover:      homekitctrl.DeviceByID,
		connect: func(ctx context.Context, host string, identity *homekitctrl.ControllerIdentity, accessory *homekitctrl.AccessoryRecord) (hapController, error) {
			return homekitctrl.Connect(ctx, host, identity, accessory)
		},
	}
}

// configureEvents wires up event handling. See the ecobeeConn field doc comments for why this
// isn't folded into newEcobeeConn.
func (c *ecobeeConn) configureEvents(watchedIDs func() []homekitctrl.CharID, onEvent func([]homekitctrl.CharacteristicValue), onDisconnect func(error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.watchedIDs, c.onEvent, c.onDisconnect = watchedIDs, onEvent, onDisconnect
}

// ensureConnected returns the current live Controller, connecting (or reconnecting) first if
// needed. Callers that get a read/write error from the returned Controller should call
// invalidate so the next call here reconnects from scratch rather than retrying a dead session.
func (c *ecobeeConn) ensureConnected(ctx context.Context) (hapController, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ctrl != nil {
		return c.ctrl, nil
	}

	identity, err := c.store.ControllerIdentity()
	if err != nil {
		return nil, fmt.Errorf("load controller identity: %w", err)
	}
	if identity == nil {
		return nil, fmt.Errorf("no controller identity in pairing store - run cmd/pair first")
	}

	accessory, err := c.store.Accessory(c.accessoryName)
	if err != nil {
		return nil, fmt.Errorf("load accessory pairing: %w", err)
	}

	discovered, err := c.discover(ctx, accessory.PairingID, discoverTimeout)
	if err != nil {
		return nil, fmt.Errorf("discover %s: %w", c.accessoryName, err)
	}
	addr, err := discovered.Addr()
	if err != nil {
		return nil, err
	}

	ctrl, err := c.connect(ctx, addr, identity, accessory)
	if err != nil {
		return nil, fmt.Errorf("connect to %s at %s: %w", c.accessoryName, addr, err)
	}

	c.ctrl = ctrl

	// Subscriptions are accessory-session state, not persistent, so every fresh connection needs
	// to re-arm them - this runs on every reconnect, not just the first. Fired off in its own
	// goroutine, on its own bounded context independent of ctx (see subscribeTimeout's doc
	// comment): a slow accessory subscribing shouldn't eat into the caller's own budget for the
	// ReadCharacteristics/WriteCharacteristics call it actually asked for, and a failure here
	// doesn't invalidate the connection either - this connection is still perfectly usable for
	// polling (Refresh's ticker keeps working), so this degrades to poll-only rather than taking
	// the bridge down over it, matching how readLiveTargetMode's live-read failure in bridge.go
	// degrades to the cached value instead of failing the command outright.
	if c.onEvent != nil {
		go func() {
			subCtx, cancel := context.WithTimeout(context.Background(), subscribeTimeout)
			defer cancel()

			// onDisconnect is wrapped here, not passed straight through, so that a stale
			// notification from a Controller that's already been replaced (e.g. it failed and a
			// newer connection has since taken over) can't tear down the newer one - invalidate
			// only acts if ctrl is still the active connection at the time this fires.
			err := ctrl.Subscribe(subCtx, c.watchedIDs(), c.onEvent, func(err error) {
				c.invalidate(ctrl)
				if c.onDisconnect != nil {
					c.onDisconnect(err)
				}
			})
			if err != nil {
				c.logger.Warn("unable to subscribe to ecobee characteristic events, falling back to poll-only", zap.Error(err))
			}
		}()
	}

	return ctrl, nil
}

// invalidate closes and drops ctrl, but only if it's still the active connection - the caller
// must pass the specific hapController instance it observed failing rather than assuming
// "whatever's current" is the right thing to close. This matters most for the async onDisconnect
// path above: a Controller's background reader can report disconnection well after the fact (this
// accessory is documented to drop connections silently, relying on TCP KeepAlive to eventually
// surface it), by which point a different caller's own error-handling may have already reconnected
// to a healthy replacement - closing unconditionally would tear that replacement down too. A
// stale/already-replaced ctrl is silently ignored rather than acted on.
func (c *ecobeeConn) invalidate(ctrl hapController) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ctrl == ctrl {
		c.ctrl.Close()
		c.ctrl = nil
	}
}
