// Package housestate implements policy.HomeAPI's "occupied"/"mode"
// GetHouseState/SetHouseState keys against a live HouseService connection.
// HouseService has no Building-level update stream yet (StreamHouseUpdates
// only reports per-room Properties - see api/house.proto), so Adapter polls
// GetBuilding on an interval instead of subscribing to a stream the way
// bridgehome does for device state, caching the result and publishing
// policy.HouseStateChangedTopic on the engine's Bus whenever it polls so the
// "sys.occupied" condition type (and any other condition built on
// GetHouseState) can react without polling itself.
package housestate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/policy"
)

// defaultPollInterval is how often Adapter polls GetBuilding, absent
// WithPollInterval.
const defaultPollInterval = 10 * time.Second

// rpcTimeout bounds each individual GetBuilding/SetHouseMode RPC, the same
// role bridgehome.commandTimeout plays for ExecuteCommand: without a
// deadline a wedged house service would hang the poll loop, or a calling
// policy script's SetHouseState call, forever.
const rpcTimeout = 10 * time.Second

// ErrNotReady is returned by GetHouseState for "occupied"/"mode" before the
// first poll has completed, and by SetHouseState("mode", ...) before Start
// has been called at all. Every other GetHouseState/SetHouseState key is
// delegated to the wrapped HomeAPI regardless of readiness.
var ErrNotReady = errors.New("housestate: adapter not started")

// Option configures optional Adapter behaviour at construction time.
type Option func(*Adapter)

// WithPollInterval overrides defaultPollInterval.
func WithPollInterval(d time.Duration) Option {
	return func(a *Adapter) { a.pollInterval = d }
}

// Adapter is a policy.HomeAPI that answers GetHouseState's "occupied" and
// "mode" keys from a polled HouseService connection, backs SetHouseState's
// "mode" key with the SetHouseMode RPC, and delegates every other
// GetHouseState/SetHouseState key - and every other HomeAPI method entirely,
// via embedding - to the wrapped HomeAPI.
type Adapter struct {
	policy.HomeAPI
	logger       *zap.Logger
	client       api2.HouseServiceClient
	buildingID   string
	pollInterval time.Duration

	mu       sync.Mutex
	ctx      context.Context
	engine   *policy.Engine
	have     bool
	occupied bool
	mode     string
}

// New creates an Adapter answering for buildingID over client, wrapping home
// for every other HomeAPI call. It does nothing until Start is called.
func New(logger *zap.Logger, client api2.HouseServiceClient, buildingID string, home policy.HomeAPI, opts ...Option) *Adapter {
	a := &Adapter{
		HomeAPI:      home,
		logger:       logger,
		client:       client,
		buildingID:   buildingID,
		pollInterval: defaultPollInterval,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Start records engine (for Bus access) and ctx (bounding every RPC Adapter
// issues afterwards, e.g. SetHouseState), polls GetBuilding once
// immediately so GetHouseState has a cached answer by the time Start
// returns, then continues polling every pollInterval until ctx is done.
func (a *Adapter) Start(ctx context.Context, engine *policy.Engine) {
	a.mu.Lock()
	a.ctx = ctx
	a.engine = engine
	a.mu.Unlock()

	a.poll(ctx)
	go a.run(ctx)
}

// run drives the periodic poll loop; the first poll happens synchronously in
// Start, not here.
func (a *Adapter) run(ctx context.Context) {
	ticker := time.NewTicker(a.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.poll(ctx)
		}
	}
}

// poll fetches buildingID's current Building and caches its State. A failed
// poll logs and holds the last known state rather than erroring GetHouseState
// callers, the same convention bridgehome's cached device reads follow
// across a disconnect.
func (a *Adapter) poll(ctx context.Context) {
	pollCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	b, err := a.client.GetBuilding(pollCtx, &api2.GetBuildingRequest{Id: a.buildingID})
	if err != nil {
		a.logger.Warn("housestate: polling building failed, holding last known state",
			zap.String("building_id", a.buildingID), zap.Error(err))
		return
	}

	a.applyBuilding(b)
}

// applyBuilding caches b's State and publishes policy.HouseStateChangedTopic
// - unconditionally, even if nothing actually changed since the last call,
// the same "it's a signal to re-check, not itself a fact" convention
// policy.Engine.UpdateDeviceState uses for "device.updated.<id>". Shared by
// poll and SetHouseState, whose own SetHouseMode response is already the
// building's new State, so there's no need to wait for the next poll to see
// a mode change take effect.
func (a *Adapter) applyBuilding(b *api2.Building) {
	a.mu.Lock()
	a.have = true
	a.occupied = b.GetState().GetOccupied()
	a.mode = b.GetState().GetMode()
	engine := a.engine
	a.mu.Unlock()

	if engine != nil {
		engine.Bus().Publish(policy.Event{Topic: policy.HouseStateChangedTopic})
	}
}

// GetHouseState implements policy.HomeAPI, answering "occupied" and "mode"
// from the most recent poll (or SetHouseState call), and delegating every
// other key - including "location.*" - to the wrapped HomeAPI.
func (a *Adapter) GetHouseState(key string) (any, error) {
	switch key {
	case "occupied":
		a.mu.Lock()
		defer a.mu.Unlock()
		if !a.have {
			return nil, ErrNotReady
		}
		return a.occupied, nil
	case "mode":
		a.mu.Lock()
		defer a.mu.Unlock()
		if !a.have {
			return nil, ErrNotReady
		}
		return a.mode, nil
	}
	return a.HomeAPI.GetHouseState(key)
}

// SetHouseState implements policy.HomeAPI. Only "mode" is backed here:
// "occupied" is computed server-side (see api/house.proto's Building.State
// doc comment) with no RPC to set it directly, so it - like every other key
// - falls through to the wrapped HomeAPI, which absent some future HomeAPI
// that claims it, means policy.ErrNotImplemented.
func (a *Adapter) SetHouseState(key string, value any) error {
	if key != "mode" {
		return a.HomeAPI.SetHouseState(key, value)
	}

	mode, ok := value.(string)
	if !ok {
		return fmt.Errorf("housestate: setHouseState(\"mode\", ...): value must be a string, got %T", value)
	}

	a.mu.Lock()
	ctx := a.ctx
	a.mu.Unlock()
	if ctx == nil {
		return ErrNotReady
	}

	setCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	b, err := a.client.SetHouseMode(setCtx, &api2.SetHouseModeRequest{BuildingId: a.buildingID, Mode: mode})
	if err != nil {
		return fmt.Errorf("housestate: setHouseMode(%q): %w", mode, err)
	}

	a.applyBuilding(b)
	return nil
}

var _ policy.HomeAPI = (*Adapter)(nil)
