package main

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/rmrobinson/house/bridges/lib/homekitctrl"
)

// fakeController is an in-memory hapController used to test EcobeeBridge/ecobeeConn's
// orchestration logic (Refresh, ProcessCommand, reconnect-on-failure) without a real network
// listener. The wire protocol itself (pair-verify, framing, timeouts) is already covered by
// homekitctrl's own tests against a fake accessory - this fake only needs to behave like
// *homekitctrl.Controller's read/write/close contract, not actually speak HAP.
type fakeController struct {
	mu     sync.Mutex
	values map[homekitctrl.CharID]any
	writes []homekitctrl.CharacteristicWrite

	readErr  error
	writeErr error
	closed   bool
}

func newFakeController() *fakeController {
	return &fakeController{values: map[homekitctrl.CharID]any{}}
}

func (f *fakeController) set(id homekitctrl.CharID, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[id] = v
}

func (f *fakeController) delete(id homekitctrl.CharID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.values, id)
}

func (f *fakeController) ReadCharacteristics(ctx context.Context, ids []homekitctrl.CharID) ([]homekitctrl.CharacteristicValue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.readErr != nil {
		return nil, f.readErr
	}

	var out []homekitctrl.CharacteristicValue
	for _, id := range ids {
		v, ok := f.values[id]
		if !ok {
			continue // simulates the characteristic being missing from an otherwise-ok response
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out = append(out, homekitctrl.CharacteristicValue{AccessoryID: id.AccessoryID, CharacteristicID: id.CharacteristicID, Value: b})
	}
	return out, nil
}

func (f *fakeController) WriteCharacteristics(ctx context.Context, writes []homekitctrl.CharacteristicWrite) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.writeErr != nil {
		return f.writeErr
	}

	f.writes = append(f.writes, writes...)
	for _, w := range writes {
		f.values[homekitctrl.CharID{AccessoryID: w.AccessoryID, CharacteristicID: w.CharacteristicID}] = w.Value
	}
	return nil
}

func (f *fakeController) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}
