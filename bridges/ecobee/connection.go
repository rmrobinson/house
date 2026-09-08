package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rmrobinson/house/bridges/lib/homekitctrl"
)

const discoverTimeout = 10 * time.Second

// hapController is the subset of *homekitctrl.Controller's API that ecobeeConn/EcobeeBridge
// depend on. Abstracted so tests can exercise the reconnect and command-routing logic in this
// package against a fake, without a real network listener - the wire protocol itself (pair-verify,
// framing, timeouts) is already covered by homekitctrl's own tests against a fake accessory, so
// there's no value in re-proving that here too.
type hapController interface {
	ReadCharacteristics(ctx context.Context, ids []homekitctrl.CharID) ([]homekitctrl.CharacteristicValue, error)
	WriteCharacteristics(ctx context.Context, writes []homekitctrl.CharacteristicWrite) error
	Close() error
}

// ecobeeConn manages a single Controller connection to the paired ecobee, transparently
// reconnecting whenever an operation reports the connection is no longer good. Every reconnect
// re-runs mDNS discovery rather than trusting a cached address, since ecobee's HAP server is
// documented to drop connections and change IPs (DHCP) more often than most HAP accessories -
// see the design notes in README.
type ecobeeConn struct {
	store         homekitctrl.Store
	accessoryName string

	// discover/connect default to the real homekitctrl package calls (set in newEcobeeConn);
	// tests override them with fakes to exercise ensureConnected/invalidate's decisions without
	// a real network.
	discover func(ctx context.Context, pairingID string, timeout time.Duration) (*homekitctrl.DiscoveredAccessory, error)
	connect  func(ctx context.Context, host string, identity *homekitctrl.ControllerIdentity, accessory *homekitctrl.AccessoryRecord) (hapController, error)

	mu   sync.Mutex
	ctrl hapController
}

func newEcobeeConn(store homekitctrl.Store, accessoryName string) *ecobeeConn {
	return &ecobeeConn{
		store:         store,
		accessoryName: accessoryName,
		discover:      homekitctrl.DeviceByID,
		connect: func(ctx context.Context, host string, identity *homekitctrl.ControllerIdentity, accessory *homekitctrl.AccessoryRecord) (hapController, error) {
			return homekitctrl.Connect(ctx, host, identity, accessory)
		},
	}
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
	return ctrl, nil
}

// invalidate closes and drops the current connection. The next ensureConnected call reconnects
// from scratch: fresh mDNS discovery, fresh pair-verify.
func (c *ecobeeConn) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ctrl != nil {
		c.ctrl.Close()
		c.ctrl = nil
	}
}
