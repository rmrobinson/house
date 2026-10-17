package metrics

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
)

var backfillNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

// historyDevice is a sensor advertising history (24h window, 5m samples).
func historyDevice(id string) *apiDevice.Device {
	d := sensorDevice(id, 600, 21)
	d.GetSensor().Metadata.History = &apiDevice.Sensor_Metadata_History{
		MaxWindow:      durationpb.New(24 * time.Hour),
		SampleInterval: durationpb.New(5 * time.Minute),
	}
	return d
}

func historySample(at time.Time, co2 int32) *api2.HistorySample {
	return &api2.HistorySample{
		Time: timestamppb.New(at),
		Snapshot: &apiDevice.Device{
			Id: "s1",
			Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
				AirQuality: &apiTrait.AirQuality{State: &apiTrait.AirQuality_State{Co2Ppm: ptr(co2)}},
			}},
		},
	}
}

// fakeHistoryServer serves GetDeviceHistory from a fixed sample list and
// records the requests it received.
type fakeHistoryServer struct {
	api2.UnimplementedBridgeServiceServer

	mu      sync.Mutex
	samples []*api2.HistorySample
	err     error
	reqs    []*api2.GetDeviceHistoryRequest
}

func (f *fakeHistoryServer) GetDeviceHistory(req *api2.GetDeviceHistoryRequest, stream api2.BridgeService_GetDeviceHistoryServer) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	samples, err := f.samples, f.err
	f.mu.Unlock()

	if err != nil {
		return err
	}
	for _, s := range samples {
		if err := stream.Send(s); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeHistoryServer) requests() []*api2.GetDeviceHistoryRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*api2.GetDeviceHistoryRequest(nil), f.reqs...)
}

type fakeWatermarks struct {
	at  time.Time
	ok  bool
	err error
	// notBefore is the lower bound the backfiller asked with.
	notBefore time.Time
}

func (f *fakeWatermarks) LatestBackfill(_ context.Context, _ string, notBefore time.Time) (time.Time, bool, error) {
	f.notBefore = notBefore
	return f.at, f.ok, f.err
}

type backfillHarness struct {
	b    *Backfiller
	svc  *Service
	sink *recordingSink
	srv  *fakeHistoryServer
	wm   *fakeWatermarks
}

func newBackfillHarness(t *testing.T) *backfillHarness {
	t.Helper()

	srv := &fakeHistoryServer{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	api2.RegisterBridgeServiceServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	client := api2.NewBridgeServiceClient(conn)

	sink := &recordingSink{}
	svc := NewService(zaptest.NewLogger(t), sink, NewSampler(DefaultSampling()), fixedRooms{"s1": "room-1"})
	svc.HandleUpdate(&api2.Update{
		Action: api2.Update_INITIAL,
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{Device: historyDevice("s1"), DeviceId: "s1", BridgeId: "b1"}},
	})
	sink.points = nil // discard the live points; only backfill output matters here

	wm := &fakeWatermarks{}
	b := NewBackfiller(zaptest.NewLogger(t), BackfillConfig{Timeout: 5 * time.Second}, svc, sink, wm,
		func() api2.BridgeServiceClient { return client })
	b.now = func() time.Time { return backfillNow }

	return &backfillHarness{b: b, svc: svc, sink: sink, srv: srv, wm: wm}
}

func TestHistoryDevicesRegistry(t *testing.T) {
	h := newBackfillHarness(t)

	devs := h.svc.HistoryDevices()
	require.Len(t, devs, 1)
	assert.Equal(t, HistoryDevice{
		DeviceID: "s1", BridgeID: "b1", Name: "Office sensor",
		MaxWindow: 24 * time.Hour, SampleInterval: 5 * time.Minute,
	}, devs[0])

	// A device that stops advertising history is dropped...
	plain := sensorDevice("s1", 600, 21)
	h.svc.HandleUpdate(&api2.Update{
		Action: api2.Update_CHANGED,
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{Device: plain, DeviceId: "s1", BridgeId: "b1"}},
	})
	assert.Empty(t, h.svc.HistoryDevices())

	// ...and so is a removed one.
	h.svc.HandleUpdate(&api2.Update{
		Action: api2.Update_ADDED,
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{Device: historyDevice("s1"), DeviceId: "s1", BridgeId: "b1"}},
	})
	require.Len(t, h.svc.HistoryDevices(), 1)
	h.svc.HandleUpdate(&api2.Update{
		Action: api2.Update_REMOVED,
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{DeviceId: "s1", BridgeId: "b1"}},
	})
	assert.Empty(t, h.svc.HistoryDevices())
}

func TestBackfillWithoutWatermarkStartsAtMaxWindow(t *testing.T) {
	h := newBackfillHarness(t)
	t1 := backfillNow.Add(-2 * time.Hour)
	t2 := backfillNow.Add(-time.Hour)
	h.srv.samples = []*api2.HistorySample{historySample(t1, 500), historySample(t2, 510)}

	written := h.b.RunOnce(context.Background())
	assert.Equal(t, 2, written)

	reqs := h.srv.requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "s1", reqs[0].GetDeviceId())
	assert.True(t, backfillNow.Add(-24*time.Hour).Equal(reqs[0].GetSince().AsTime()))
	assert.True(t, backfillNow.Equal(reqs[0].GetUntil().AsTime()))
	assert.True(t, backfillNow.Add(-24*time.Hour).Equal(h.wm.notBefore))

	require.Len(t, h.sink.points, 2)
	p := h.sink.points[0]
	assert.Equal(t, "state", p.Measurement)
	assert.Equal(t, map[string]string{
		"bridge_id": "b1", "device_id": "s1", "device_name": "Office sensor",
		"device_label": "Office sensor (Study)",
		"room_id":      "room-1", "trait": "air_quality", "source": "backfill",
	}, p.Tags)
	assert.True(t, t1.Equal(p.Time))
	assert.Equal(t, FieldValue{Kind: KindInt, Int: 500}, p.Fields["co2_ppm"])
}

func TestBackfillResumesFromWatermark(t *testing.T) {
	h := newBackfillHarness(t)
	wm := backfillNow.Add(-3 * time.Hour)
	h.wm.at, h.wm.ok = wm, true

	h.b.RunOnce(context.Background())

	reqs := h.srv.requests()
	require.Len(t, reqs, 1)
	assert.True(t, wm.Equal(reqs[0].GetSince().AsTime()), "since=%s", reqs[0].GetSince().AsTime())
}

func TestBackfillSkipsWhenNothingNew(t *testing.T) {
	h := newBackfillHarness(t)
	// Less than one sample interval since the last backfilled sample.
	h.wm.at, h.wm.ok = backfillNow.Add(-2*time.Minute), true

	assert.Zero(t, h.b.RunOnce(context.Background()))
	assert.Empty(t, h.srv.requests(), "no BLE session should be spent")
}

func TestBackfillSkipsDeviceWhenWatermarkUnreadable(t *testing.T) {
	h := newBackfillHarness(t)
	h.wm.err = errors.New("influx down")

	assert.Zero(t, h.b.RunOnce(context.Background()))
	assert.Empty(t, h.srv.requests(), "must not refetch the full window blind")
}

func TestBackfillDeviceFailureIsNotFatal(t *testing.T) {
	h := newBackfillHarness(t)
	h.svc.HandleUpdate(&api2.Update{
		Action: api2.Update_INITIAL,
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{Device: historyDevice("s2"), DeviceId: "s2", BridgeId: "b1"}},
	})
	h.sink.points = nil
	h.srv.err = status.Error(codes.Unavailable, "ble connect failed")

	assert.Zero(t, h.b.RunOnce(context.Background()))
	assert.Len(t, h.srv.requests(), 2, "second device is still attempted")

	// It recovers on the next pass.
	h.srv.mu.Lock()
	h.srv.err = nil
	h.srv.samples = []*api2.HistorySample{historySample(backfillNow.Add(-time.Hour), 480)}
	h.srv.mu.Unlock()
	assert.Equal(t, 2, h.b.RunOnce(context.Background()))
}

func TestBackfillNotConnected(t *testing.T) {
	h := newBackfillHarness(t)
	h.b.client = func() api2.BridgeServiceClient { return nil }

	assert.Zero(t, h.b.RunOnce(context.Background()))
}

func TestHistoryPointsIgnoresIncompleteSamples(t *testing.T) {
	h := newBackfillHarness(t)
	dev := h.svc.HistoryDevices()[0]
	assert.Empty(t, h.svc.HistoryPoints(dev, &api2.HistorySample{}))
	assert.Empty(t, h.svc.HistoryPoints(dev, &api2.HistorySample{Time: timestamppb.Now()}))
}

func TestInfluxQuerierLatestBackfill(t *testing.T) {
	var gotBody, gotAuth, gotPath string
	respond := func(body string, code int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b := make([]byte, 4096)
			n, _ := r.Body.Read(b)
			gotBody, gotAuth, gotPath = string(b[:n]), r.Header.Get("Authorization"), r.URL.Path
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}))
	}
	cfg := func(url string) InfluxConfig {
		return InfluxConfig{URL: url, Org: "house", Bucket: "my\"bucket", Token: "tok"}
	}
	notBefore := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

	// A result: annotation-free CSV with its own header.
	srv := respond(",result,table,_start,_stop,_time,_value\n,_result,0,x,y,2026-10-12T11:55:00.5Z,500\n\n", 200)
	q, err := NewInfluxQuerier(cfg(srv.URL))
	require.NoError(t, err)
	at, ok, err := q.LatestBackfill(context.Background(), `dev"$1`, notBefore)
	srv.Close()
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, time.Date(2026, 10, 12, 11, 55, 0, 500_000_000, time.UTC).Equal(at))
	assert.Equal(t, "/api/v2/query", gotPath)
	assert.Equal(t, "Token tok", gotAuth)
	// Values are escaped rather than spliced raw into the query.
	assert.Contains(t, gotBody, `r.device_id == "dev\"\$1"`)
	assert.Contains(t, gotBody, `from(bucket: "my\"bucket")`)
	assert.Contains(t, gotBody, `range(start: 2026-10-11T12:00:00Z)`)
	assert.Contains(t, gotBody, `r.source == "backfill"`)
	assert.Contains(t, gotBody, `keep(columns: ["_time"])`, "mixed-type fields must not reach max()")

	// No matching points: empty body.
	srv = respond("", 200)
	q, _ = NewInfluxQuerier(cfg(srv.URL))
	_, ok, err = q.LatestBackfill(context.Background(), "d", notBefore)
	srv.Close()
	require.NoError(t, err)
	assert.False(t, ok)

	// Errors surface.
	srv = respond("nope", 401)
	q, _ = NewInfluxQuerier(cfg(srv.URL))
	_, _, err = q.LatestBackfill(context.Background(), "d", notBefore)
	srv.Close()
	assert.Error(t, err)
}
