// Package housestate implements policy.HomeAPI's "occupied"/"mode"
// GetHouseState/SetHouseState keys against a live HouseService connection.
// Adapter subscribes to HouseService.StreamHouseUpdates for buildingID,
// caching its BuildingUpdate branch's State (ignoring the RoomUpdate branch
// entirely - that's not this package's concern) and publishing
// policy.HouseStateChangedTopic on the engine's Bus whenever one arrives, so
// the "sys.occupied" condition type (and any other condition built on
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
	"github.com/rmrobinson/house/service/lib/bridgeconn"
	"github.com/rmrobinson/house/service/policy"
)

// rpcTimeout bounds the SetHouseMode RPC a policy script's SetHouseState
// call issues - without a deadline a wedged house service would hang it
// forever. The long-lived StreamHouseUpdates call itself is bounded by ctx
// (Start's, cancelled for the engine's own lifetime), not this.
const rpcTimeout = 10 * time.Second

// ErrNotReady is returned by GetHouseState for "occupied"/"mode" before the
// stream's first BuildingUpdate has arrived, and by SetHouseState("mode",
// ...) before Start has been called at all. Every other
// GetHouseState/SetHouseState key is delegated to the wrapped HomeAPI
// regardless of readiness.
var ErrNotReady = errors.New("housestate: adapter not started")

// Adapter is a policy.HomeAPI that answers GetHouseState's "occupied" and
// "mode" keys from a subscribed HouseService connection, backs
// SetHouseState's "mode" key with the SetHouseMode RPC, and delegates every
// other GetHouseState/SetHouseState key - and every other HomeAPI method
// entirely, via embedding - to the wrapped HomeAPI.
type Adapter struct {
	policy.HomeAPI
	logger     *zap.Logger
	client     api2.HouseServiceClient
	buildingID string
	backoff    *bridgeconn.Backoff

	mu       sync.Mutex
	ctx      context.Context
	engine   *policy.Engine
	have     bool
	occupied bool
	mode     string
}

// New creates an Adapter answering for buildingID over client, wrapping home
// for every other HomeAPI call. It does nothing until Start is called.
func New(logger *zap.Logger, client api2.HouseServiceClient, buildingID string, home policy.HomeAPI) *Adapter {
	return &Adapter{
		HomeAPI:    home,
		logger:     logger,
		client:     client,
		buildingID: buildingID,
		backoff:    bridgeconn.NewBackoff(),
	}
}

// Start records engine (for Bus access) and ctx (bounding every RPC Adapter
// issues afterwards, e.g. SetHouseState), then begins subscribing to
// StreamHouseUpdates in the background, reconnecting with jittered
// exponential backoff whenever the stream drops. It returns immediately -
// GetHouseState returns ErrNotReady until the subscription's first
// BuildingUpdate arrives, the same "connect in the background, readiness
// comes later" contract bridgehome.Adapter.Start follows for BridgeService.
func (a *Adapter) Start(ctx context.Context, engine *policy.Engine) {
	a.mu.Lock()
	a.ctx = ctx
	a.engine = engine
	a.mu.Unlock()

	go bridgeconn.Retry(ctx, a.backoff, func(err error) {
		a.logger.Warn("housestate: stream ended, will retry",
			zap.String("building_id", a.buildingID), zap.Error(err))
	}, a.streamOnce)
}

// streamOnce subscribes to StreamHouseUpdates for buildingID and applies
// every BuildingUpdate it carries (ignoring RoomUpdate ones) until the
// stream ends or errors.
func (a *Adapter) streamOnce(ctx context.Context, backoff *bridgeconn.Backoff) error {
	stream, err := a.client.StreamHouseUpdates(ctx, &api2.StreamHouseUpdatesRequest{BuildingId: a.buildingID})
	if err != nil {
		return fmt.Errorf("stream house updates: %w", err)
	}

	first := true
	for {
		update, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}

		if first {
			// The connection is only confirmed live once a message has
			// actually been received from it, the same reasoning
			// bridgeconn.Conn.connectOnce documents for resetting here
			// rather than right after the StreamHouseUpdates call.
			backoff.Reset()
			first = false
		}

		if building := update.GetBuilding(); building != nil {
			a.applyState(building.GetState())
		}
	}
}

// applyState caches state and publishes policy.HouseStateChangedTopic -
// unconditionally, even if nothing actually changed since the last call,
// the same "it's a signal to re-check, not itself a fact" convention
// policy.Engine.UpdateDeviceState uses for "device.updated.<id>". Shared by
// streamOnce and SetHouseState, whose own SetHouseMode response already
// carries the building's new State, so there's no need to wait for the next
// stream message to see a mode change take effect.
func (a *Adapter) applyState(state *api2.Building_State) {
	a.mu.Lock()
	a.have = true
	a.occupied = state.GetOccupied()
	a.mode = state.GetMode()
	engine := a.engine
	a.mu.Unlock()

	if engine != nil {
		engine.Bus().Publish(policy.Event{Topic: policy.HouseStateChangedTopic})
	}
}

// GetHouseState implements policy.HomeAPI, answering "occupied" and "mode"
// from the most recent BuildingUpdate (or SetHouseState call), and
// delegating every other key - including "location.*" - to the wrapped
// HomeAPI.
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

	a.applyState(b.GetState())
	return nil
}

var _ policy.HomeAPI = (*Adapter)(nil)
