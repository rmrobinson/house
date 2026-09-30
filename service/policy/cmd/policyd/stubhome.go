package main

import (
	"sync"

	"github.com/rmrobinson/house/service/policy"
	"go.uber.org/zap"
)

// stubHomeAPI is a placeholder policy.HomeAPI: it keeps state purely in
// memory, logs every call, and doesn't talk to any real device or house
// service. It exists so policyd is runnable and its UI is exercisable end
// to end (registering policies, watching them fire, browsing execution
// logs) before the real HomeAPI adapter — wiring device/house state to
// actual bridge gRPC clients, per AGENTS.md's service/policy entry — is
// built. Swap it out for that adapter once it exists; nothing else in
// main.go should need to change.
type stubHomeAPI struct {
	logger *zap.Logger

	mu         sync.Mutex
	lights     map[string]bool
	sensors    map[string]float64
	attributes map[string]map[string]any
	houseState map[string]any
}

func newStubHomeAPI(logger *zap.Logger) *stubHomeAPI {
	return &stubHomeAPI{
		logger:     logger,
		lights:     make(map[string]bool),
		sensors:    make(map[string]float64),
		attributes: make(map[string]map[string]any),
		houseState: make(map[string]any),
	}
}

func (h *stubHomeAPI) GetLight(id string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	on := h.lights[id]
	h.logger.Debug("stub home: GetLight", zap.String("id", id), zap.Bool("on", on))
	return on, nil
}

func (h *stubHomeAPI) SetLight(id string, on bool) error {
	h.mu.Lock()
	h.lights[id] = on
	h.mu.Unlock()
	h.logger.Info("stub home: SetLight", zap.String("id", id), zap.Bool("on", on))
	return nil
}

func (h *stubHomeAPI) GetSensor(id string) (float64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.sensors[id]
	h.logger.Debug("stub home: GetSensor", zap.String("id", id), zap.Float64("value", v))
	return v, nil
}

func (h *stubHomeAPI) GetState(id, key string) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.attributes[id][key]
	h.logger.Debug("stub home: GetState", zap.String("id", id), zap.String("key", key))
	return v, nil
}

func (h *stubHomeAPI) SetState(id, key string, value any) error {
	h.mu.Lock()
	attrs, ok := h.attributes[id]
	if !ok {
		attrs = make(map[string]any)
		h.attributes[id] = attrs
	}
	attrs[key] = value
	h.mu.Unlock()
	h.logger.Info("stub home: SetState", zap.String("id", id), zap.String("key", key))
	return nil
}

func (h *stubHomeAPI) GetHouseState(key string) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.houseState[key]
	h.logger.Debug("stub home: GetHouseState", zap.String("key", key))
	return v, nil
}

func (h *stubHomeAPI) SetHouseState(key string, value any) error {
	h.mu.Lock()
	h.houseState[key] = value
	h.mu.Unlock()
	h.logger.Info("stub home: SetHouseState", zap.String("key", key))
	return nil
}

// GetLastKnown always returns "no entry": the stub has no real device
// history to draw on, so e.g. sys.power-restore's script correctly finds
// nothing to restore rather than fabricating a value.
func (h *stubHomeAPI) GetLastKnown(id string) (any, error) {
	h.logger.Debug("stub home: GetLastKnown", zap.String("id", id))
	return nil, nil
}

func (h *stubHomeAPI) Notify(event string, payload map[string]any) error {
	h.logger.Info("stub home: Notify", zap.String("event", event), zap.Any("payload", payload))
	return nil
}

var _ policy.HomeAPI = (*stubHomeAPI)(nil)
