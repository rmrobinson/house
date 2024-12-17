package main

import (
	"context"
	"sync"

	"go.uber.org/zap"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

// ChargerBridge acts as the handler for Bridge requests for this charger.
type ChargerBridge struct {
	logger *zap.Logger

	charger *Charger
	svc     *bridge.Service
	dLock   *sync.Mutex
}

// NewChargerBridge creates a new charger bridge
func NewChargerBridge(logger *zap.Logger, charger *Charger) *ChargerBridge {
	return &ChargerBridge{
		logger:  logger,
		charger: charger,
		svc:     nil,
		dLock:   &sync.Mutex{},
	}
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (cb *ChargerBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	cb.logger.Error("received unsupported command - shouldn't happen")
	return nil, bridge.ErrUnsupportedCommand
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it does nothing
// since there isn't 'remote' state which needs to be refreshed.
func (cb *ChargerBridge) Refresh(ctx context.Context) error {
	// TODO: call to the Tesla API to refresh the local state.
	_ = cb.charger.Refresh()
	// TODO: convert err to a Bridge API error
	return nil
}

func (cb *ChargerBridge) updateDevice(d *device.Device) {
	cb.dLock.Lock()
	defer cb.dLock.Unlock()

	// If the devices are different, trigger an update to registered listeners
	// TODO: need to use a deep compare
	// TODO: this isn't right
	if d != cb.charger.deviceFromCachedState() {
		// Compute delta

		// Send update
		cb.svc.UpdateDevice(d)
	}
}
