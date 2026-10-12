package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
)

func sensorDevice(id string, co2 int32, tempC float32) *apiDevice.Device {
	return &apiDevice.Device{
		Id:     id,
		Config: &apiDevice.Device_Config{Name: "Office sensor"},
		Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
			Metadata: &apiDevice.Sensor_Metadata{LowBattery: true},
			AirQuality: &apiTrait.AirQuality{
				// Attributes must never be flattened.
				Attributes: &apiTrait.AirQuality_Attributes{AirQualities: []string{"co2"}},
				State: &apiTrait.AirQuality_State{
					AirQuality: ptr("good"),
					Co2Ppm:     ptr(co2),
				},
			},
			AirProperties: &apiTrait.AirProperties{
				State: &apiTrait.AirProperties_State{TemperatureC: tempC},
			},
			Water: &apiDevice.Sensor_BinarySensor{IsActive: true},
		}},
	}
}

func ptr[T any](v T) *T { return &v }

func leafMap(ts TraitState) map[string]FieldValue {
	m := map[string]FieldValue{}
	for _, l := range ts.Leaves {
		m[l.Name] = l.Value
	}
	return m
}

func TestFlatten(t *testing.T) {
	got := Flatten(sensorDevice("s1", 640, 21.5))

	byTrait := map[string]TraitState{}
	for _, ts := range got {
		byTrait[ts.Trait] = ts
	}

	// metadata is bookkeeping, not a trait.
	assert.NotContains(t, byTrait, "metadata")

	aq := leafMap(byTrait["air_quality"])
	assert.Equal(t, FieldValue{Kind: KindInt, Int: 640}, aq["co2_ppm"])
	// Strings (air_quality) and attributes (air_qualities list) are skipped.
	assert.NotContains(t, aq, "air_quality")
	assert.NotContains(t, aq, "air_qualities")
	// Unset optionals are absent rather than zero.
	assert.NotContains(t, aq, "radon_bq_m3")

	props := leafMap(byTrait["air_properties"])
	assert.InDelta(t, 21.5, props["temperature_c"].Float, 0.001)

	// A trait-less message has its scalars read directly.
	assert.Equal(t, FieldValue{Kind: KindBool, Bool: true}, leafMap(byTrait["water"])["is_active"])
}

func TestFlattenNoDetails(t *testing.T) {
	assert.Empty(t, Flatten(&apiDevice.Device{Id: "x"}))
}

func TestPointLine(t *testing.T) {
	p := Point{
		Measurement: "state",
		Tags:        map[string]string{"device_name": "Living Room, Lamp", "room_id": "", "b": "x"},
		Fields: map[string]FieldValue{
			"temp":  {Kind: KindFloat, Float: 21.5},
			"co2":   {Kind: KindInt, Int: 600},
			"on":    {Kind: KindBool, Bool: true},
			"quote": {Kind: KindString, Str: `a "b"`},
		},
		Time: time.Unix(10, 5),
	}
	assert.Equal(t,
		`state,b=x,device_name=Living\ Room\,\ Lamp co2=600i,on=true,quote="a \"b\"",temp=21.5 10000000005`,
		p.Line())

	assert.Equal(t, "", Point{Measurement: "state"}.Line())
}

func TestSampler(t *testing.T) {
	cfg := DefaultSampling()
	s := NewSampler(cfg)
	t0 := time.Unix(1000, 0)

	co2 := func(v int64) []Leaf {
		return []Leaf{{Name: "co2_ppm", Continuous: true, Value: FieldValue{Kind: KindInt, Int: v}}}
	}

	// First sighting is written.
	assert.Len(t, s.Filter("d", "air_quality", co2(600), t0), 1)
	// Within the 10ppm deadband: dropped.
	assert.Empty(t, s.Filter("d", "air_quality", co2(605), t0.Add(time.Second)))
	// Deadband is measured against the last *written* value, not the last seen:
	// 605 was dropped, so 611 is +11 from 600 and written.
	assert.Len(t, s.Filter("d", "air_quality", co2(611), t0.Add(2*time.Second)), 1)
	// Unchanged until the heartbeat elapses, then written.
	assert.Empty(t, s.Filter("d", "air_quality", co2(611), t0.Add(time.Minute)))
	assert.Len(t, s.Filter("d", "air_quality", co2(611), t0.Add(2*time.Second+5*time.Minute)), 1)

	// Different device is tracked separately.
	assert.Len(t, s.Filter("other", "air_quality", co2(611), t0), 1)

	// Discrete leaves write on any change, never on a repeat.
	on := func(v bool) []Leaf { return []Leaf{{Name: "on", Value: FieldValue{Kind: KindBool, Bool: v}}} }
	assert.Len(t, s.Filter("d", "on_off", on(true), t0), 1)
	assert.Empty(t, s.Filter("d", "on_off", on(true), t0.Add(time.Second)))
	assert.Len(t, s.Filter("d", "on_off", on(false), t0.Add(2*time.Second)), 1)

	// Forget makes the next sighting a first sighting again.
	s.Forget("d")
	assert.Len(t, s.Filter("d", "air_quality", co2(611), t0), 1)
}

func TestSamplerModeNone(t *testing.T) {
	s := NewSampler(SamplingConfig{Mode: ModeNone})
	l := []Leaf{{Name: "co2_ppm", Continuous: true, Value: FieldValue{Kind: KindInt, Int: 600}}}
	assert.Len(t, s.Filter("d", "air_quality", l, time.Unix(1, 0)), 1)
	assert.Len(t, s.Filter("d", "air_quality", l, time.Unix(2, 0)), 1)
}

func TestSamplerTraitScopedDeadband(t *testing.T) {
	s := NewSampler(SamplingConfig{
		Mode:     ModeDeadband,
		Deadband: map[string]float64{"value": 5, "temperature.value": 0.5},
	})
	l := func(v float64) []Leaf {
		return []Leaf{{Name: "value", Continuous: true, Value: FieldValue{Kind: KindFloat, Float: v}}}
	}
	assert.Len(t, s.Filter("d", "temperature", l(20), time.Unix(0, 0)), 1)
	// 0.6 clears the trait-scoped 0.5 but not the generic 5.
	assert.Len(t, s.Filter("d", "temperature", l(20.6), time.Unix(1, 0)), 1)
}

// recordingSink captures written points.
type recordingSink struct {
	mu     sync.Mutex
	points []Point
}

func (r *recordingSink) Write(points ...Point) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.points = append(r.points, points...)
}

type fixedRooms map[string]string

func (f fixedRooms) RoomID(id string) string { return f[id] }

func TestServiceStateUpdates(t *testing.T) {
	sink := &recordingSink{}
	s := NewService(zaptest.NewLogger(t), sink, NewSampler(DefaultSampling()), fixedRooms{"s1": "room-1"})
	now := time.Unix(5000, 0)
	s.now = func() time.Time { return now }

	deviceUpdate := func(d *apiDevice.Device, action api2.Update_Action) *api2.Update {
		return &api2.Update{
			Action: action,
			Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{Device: d, DeviceId: d.GetId(), BridgeId: "b1"}},
		}
	}

	s.HandleUpdate(deviceUpdate(sensorDevice("s1", 600, 21), api2.Update_INITIAL))
	require.Len(t, sink.points, 3) // air_quality, air_properties, water

	var aq Point
	for _, p := range sink.points {
		if p.Tags["trait"] == "air_quality" {
			aq = p
		}
	}
	assert.Equal(t, "state", aq.Measurement)
	assert.Equal(t, map[string]string{
		"bridge_id": "b1", "device_id": "s1", "device_name": "Office sensor",
		"room_id": "room-1", "trait": "air_quality", "source": "live",
	}, aq.Tags)
	assert.Equal(t, now, aq.Time)

	// A reconnect replay of an identical device writes nothing.
	sink.points = nil
	s.HandleUpdate(deviceUpdate(sensorDevice("s1", 600, 21), api2.Update_INITIAL))
	assert.Empty(t, sink.points)

	// Only the trait that moved past its deadband is written.
	s.HandleUpdate(deviceUpdate(sensorDevice("s1", 700, 21), api2.Update_CHANGED))
	require.Len(t, sink.points, 1)
	assert.Equal(t, "air_quality", sink.points[0].Tags["trait"])

	// Bulk initial updates are handled too, using the bridge's own ID.
	sink.points = nil
	s.HandleUpdate(&api2.Update{
		Action: api2.Update_INITIAL,
		Update: &api2.Update_InitialUpdate{InitialUpdate: &api2.InitialUpdate{
			Bridge:  &api2.Bridge{Id: "b2"},
			Devices: []*apiDevice.Device{sensorDevice("s2", 400, 19)},
		}},
	})
	require.NotEmpty(t, sink.points)
	assert.Equal(t, "b2", sink.points[0].Tags["bridge_id"])
	assert.Equal(t, "", sink.points[0].Tags["room_id"])
}

func TestServiceCommandEvent(t *testing.T) {
	sink := &recordingSink{}
	s := NewService(zaptest.NewLogger(t), sink, NewSampler(DefaultSampling()), nil)

	at := time.Unix(9000, 0)
	s.HandleUpdate(&api2.Update{
		Action: api2.Update_COMMAND,
		Update: &api2.Update_CommandEvent{CommandEvent: &api2.CommandEvent{
			BridgeId:         "b1",
			ResultingVersion: "v2",
			Time:             timestamppb.New(at),
			Result:           status.New(codes.FailedPrecondition, "version mismatch").Proto(),
			Command: &command.Command{
				Id:       "c1",
				DeviceId: "light-1",
				Origin:   "viewerui",
				Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
			},
		}},
	})

	require.Len(t, sink.points, 1)
	p := sink.points[0]
	assert.Equal(t, "command", p.Measurement)
	assert.Equal(t, map[string]string{
		"bridge_id": "b1", "device_id": "light-1", "command_type": "on_off", "origin": "viewerui",
	}, p.Tags)
	assert.True(t, at.Equal(p.Time), "got %s", p.Time)
	assert.Equal(t, int64(codes.FailedPrecondition), p.Fields["outcome"].Int)
	assert.Equal(t, "c1", p.Fields["command_id"].Str)
	assert.Equal(t, "v2", p.Fields["resulting_version"].Str)
	assert.Contains(t, p.Fields["detail"].Str, `"deviceId":"light-1"`)
}

func TestServiceRemovedForgetsSampler(t *testing.T) {
	sink := &recordingSink{}
	s := NewService(zaptest.NewLogger(t), sink, NewSampler(DefaultSampling()), nil)
	d := sensorDevice("s1", 600, 21)

	update := func(action api2.Update_Action) *api2.Update {
		return &api2.Update{
			Action: action,
			Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{Device: d, DeviceId: "s1", BridgeId: "b1"}},
		}
	}
	s.HandleUpdate(update(api2.Update_ADDED))
	n := len(sink.points)
	require.NotZero(t, n)

	s.HandleUpdate(update(api2.Update_REMOVED))
	assert.Len(t, sink.points, n, "removal writes nothing")

	s.HandleUpdate(update(api2.Update_ADDED))
	assert.Len(t, sink.points, 2*n, "re-added device is a first sighting again")
}

// influxStub is a stand-in for InfluxDB's v2 write endpoint.
type influxStub struct {
	mu      sync.Mutex
	status  int
	lines   []string
	request *http.Request
}

func (i *influxStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.request = r
	if i.status != 0 && i.status/100 != 2 {
		w.WriteHeader(i.status)
		return
	}
	i.lines = append(i.lines, strings.Split(string(body), "\n")...)
	w.WriteHeader(http.StatusNoContent)
}

func (i *influxStub) setStatus(s int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.status = s
}

func (i *influxStub) sortedLines() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := append([]string(nil), i.lines...)
	sort.Strings(out)
	return out
}

func newWriter(t *testing.T, url string, maxBuffered int) *InfluxWriter {
	t.Helper()
	w, err := NewInfluxWriter(zaptest.NewLogger(t), InfluxConfig{
		URL: url, Org: "house", Bucket: "house", Token: "secret", MaxBuffered: maxBuffered, BatchSize: 2,
	})
	require.NoError(t, err)
	return w
}

func testPoint(v int64) Point {
	return Point{
		Measurement: "state",
		Tags:        map[string]string{"device_id": "d"},
		Fields:      map[string]FieldValue{"v": {Kind: KindInt, Int: v}},
		Time:        time.Unix(v, 0),
	}
}

func TestInfluxWriterFlush(t *testing.T) {
	stub := &influxStub{}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	w := newWriter(t, srv.URL, 0)
	w.Write(testPoint(1), testPoint(2), testPoint(3), Point{Measurement: "empty"})
	w.Flush(context.Background())

	assert.Equal(t, []string{
		"state,device_id=d v=1i 1000000000",
		"state,device_id=d v=2i 2000000000",
		"state,device_id=d v=3i 3000000000",
	}, stub.sortedLines())
	assert.Equal(t, int64(3), w.Written())
	assert.Equal(t, 0, w.Pending())

	assert.Equal(t, "/api/v2/write", stub.request.URL.Path)
	assert.Equal(t, "house", stub.request.URL.Query().Get("org"))
	assert.Equal(t, "house", stub.request.URL.Query().Get("bucket"))
	assert.Equal(t, "ns", stub.request.URL.Query().Get("precision"))
	assert.Equal(t, "Token secret", stub.request.Header.Get("Authorization"))
}

func TestInfluxWriterRetriesWhenDown(t *testing.T) {
	stub := &influxStub{status: http.StatusServiceUnavailable}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	w := newWriter(t, srv.URL, 0)
	w.Write(testPoint(1), testPoint(2))
	w.Flush(context.Background())
	assert.Equal(t, 2, w.Pending(), "kept for retry")
	assert.Zero(t, w.Dropped())

	stub.setStatus(0)
	w.Flush(context.Background())
	assert.Equal(t, 0, w.Pending())
	assert.Len(t, stub.sortedLines(), 2)
}

func TestInfluxWriterDropsRejectedBatch(t *testing.T) {
	stub := &influxStub{status: http.StatusBadRequest}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	w := newWriter(t, srv.URL, 0)
	w.Write(testPoint(1), testPoint(2))
	w.Flush(context.Background())
	assert.Equal(t, 0, w.Pending(), "a rejected batch would be rejected forever")
	assert.Equal(t, int64(2), w.Dropped())
}

func TestInfluxWriterBoundedBuffer(t *testing.T) {
	// Nothing listening: points just accumulate.
	w := newWriter(t, "http://127.0.0.1:1", 3)
	for i := int64(1); i <= 5; i++ {
		w.Write(testPoint(i))
	}
	assert.Equal(t, 3, w.Pending())
	assert.Equal(t, int64(2), w.Dropped())

	// Oldest were dropped, newest kept.
	w.mu.Lock()
	defer w.mu.Unlock()
	assert.Equal(t, "state,device_id=d v=3i 3000000000", w.buf[0])
}

func TestNewInfluxWriterRequiresConfig(t *testing.T) {
	_, err := NewInfluxWriter(zaptest.NewLogger(t), InfluxConfig{URL: "http://x"})
	assert.Error(t, err)
}
