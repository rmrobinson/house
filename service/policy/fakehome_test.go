package policy

import "sync"

type notification struct {
	event   string
	payload map[string]any
}

// fakeHomeAPI is an in-memory HomeAPI used by tests. sensorErr, when set,
// makes GetSensor fail, simulating a HomeAPI-level (as opposed to
// script-level) error.
type fakeHomeAPI struct {
	mu sync.Mutex

	lights     map[string]bool
	sensors    map[string]float64
	attributes map[string]map[string]any
	names      map[string]string
	houseState map[string]any
	lastKnown  map[string]any

	sensorErr error

	notifications []notification
}

func newFakeHomeAPI() *fakeHomeAPI {
	return &fakeHomeAPI{
		lights:     make(map[string]bool),
		sensors:    make(map[string]float64),
		attributes: make(map[string]map[string]any),
		names:      make(map[string]string),
		houseState: make(map[string]any),
		lastKnown:  make(map[string]any),
	}
}

func (f *fakeHomeAPI) GetLight(id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lights[id], nil
}

func (f *fakeHomeAPI) SetLight(id string, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lights[id] = on
	return nil
}

func (f *fakeHomeAPI) GetSensor(id string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sensorErr != nil {
		return 0, f.sensorErr
	}
	return f.sensors[id], nil
}

func (f *fakeHomeAPI) GetState(id, key string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attributes[id][key], nil
}

func (f *fakeHomeAPI) SetState(id, key string, value any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	attrs, ok := f.attributes[id]
	if !ok {
		attrs = make(map[string]any)
		f.attributes[id] = attrs
	}
	attrs[key] = value
	return nil
}

func (f *fakeHomeAPI) GetDeviceName(id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name, ok := f.names[id]; ok && name != "" {
		return name, nil
	}
	return id, nil
}

func (f *fakeHomeAPI) setDeviceName(id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names[id] = name
}

func (f *fakeHomeAPI) HasState(id, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.attributes[id][key]
	return ok, nil
}

func (f *fakeHomeAPI) GetHouseState(key string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.houseState[key], nil
}

func (f *fakeHomeAPI) SetHouseState(key string, value any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.houseState[key] = value
	return nil
}

func (f *fakeHomeAPI) getHouseState(key string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.houseState[key]
}

func (f *fakeHomeAPI) GetLastKnown(id string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastKnown[id], nil
}

func (f *fakeHomeAPI) setLastKnown(id string, value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastKnown[id] = value
}

func (f *fakeHomeAPI) getLight(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lights[id]
}

func (f *fakeHomeAPI) Notify(event string, payload map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifications = append(f.notifications, notification{event: event, payload: payload})
	return nil
}

func (f *fakeHomeAPI) notifyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.notifications)
}
