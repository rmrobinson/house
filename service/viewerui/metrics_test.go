package main

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/lib/houseview"
)

// fakeMetrics serves canned history and records what it was asked.
type fakeMetrics struct {
	api2.UnimplementedMetricsServiceServer

	mu     sync.Mutex
	reqs   []*api2.GetSeriesRequest
	points []*api2.SeriesPoint
	err    error
}

func (f *fakeMetrics) GetSeries(_ context.Context, r *api2.GetSeriesRequest) (*api2.GetSeriesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	if f.err != nil {
		return nil, f.err
	}
	return &api2.GetSeriesResponse{Points: f.points}, nil
}

func (f *fakeMetrics) requests() []*api2.GetSeriesRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*api2.GetSeriesRequest(nil), f.reqs...)
}

// withMetrics starts a fake MetricsService and points s at it, with the clock
// pinned to now.
func withMetrics(t *testing.T, s *Server, now time.Time) *fakeMetrics {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	fm := &fakeMetrics{}
	gs := grpc.NewServer()
	api2.RegisterMetricsServiceServer(gs, fm)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	conn, err := grpcutil.DialInsecure(lis.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	s.metrics = newMetricsFeed(zaptest.NewLogger(t), api2.NewMetricsServiceClient(conn))
	s.metrics.now = func() time.Time { return now }
	return fm
}

func series(now time.Time, vals ...float64) []*api2.SeriesPoint {
	var out []*api2.SeriesPoint
	for i, v := range vals {
		at := now.Add(-time.Duration(len(vals)-i) * time.Hour)
		out = append(out, &api2.SeriesPoint{Time: timestamppb.New(at), Value: v})
	}
	return out
}

func TestSparkRowOnlyWhenMetricsConfiguredAndDeviceHasNumericReadings(t *testing.T) {
	s, house, _ := startTestServer(t)
	house.setExtraDevice(fanWithTemp())

	body := get(s, "/rooms/r1", "HX-Request", "true").Body.String()
	assert.NotContains(t, body, `class="sparks"`, "no metrics server, no sparklines")

	withMetrics(t, s, time.Now())
	body = get(s, "/rooms/r1", "HX-Request", "true").Body.String()
	assert.Contains(t, body, `class="sparks"`)
	assert.Contains(t, body, `hx-get="/devices/fan1/sparklines"`)
	assert.Contains(t, body, `hx-trigger="intersect once"`)
	// Only the fan has a numeric reading; the lamp and cameras don't.
	assert.Equal(t, 1, strings.Count(body, `class="sparks"`))
}

func TestSparklinesEscapeDeviceIDInHref(t *testing.T) {
	assert.Equal(t, "/devices/abc:2/sparklines", sparkHref("abc:2"))
	assert.Equal(t, "/devices/a%20b%2Fc/sparklines", sparkHref("a b/c"))
}

func TestSparklinesRendersChartsRangeLinksAndAsksForTheRightSeries(t *testing.T) {
	s, _, bridge := startTestServer(t)
	bridge.devices = map[string]*apiDevice.Device{"fan1": fanWithTemp()}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	fm := withMetrics(t, s, now)
	fm.points = series(now, 20, 22, 24)

	rec := get(s, "/devices/fan1/sparklines", "HX-Request", "true")
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "<polyline")
	assert.Contains(t, body, "Temp")
	assert.Contains(t, body, "20–24")
	for _, r := range []string{"24h", "7d", "30d"} {
		assert.Contains(t, body, `hx-get="/devices/fan1/sparklines?range=`+r+`"`)
	}
	assert.Contains(t, body, `aria-pressed="true"`)

	reqs := fm.requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "fan1", reqs[0].GetDeviceId())
	assert.Equal(t, "temperature.value", reqs[0].GetAttribute())
	assert.EqualValues(t, sparkPoints, reqs[0].GetMaxPoints())
	assert.Equal(t, now.Add(-24*time.Hour), reqs[0].GetStart().AsTime())
}

func TestSparklinesRangeSelectsTheWindow(t *testing.T) {
	s, _, bridge := startTestServer(t)
	bridge.devices = map[string]*apiDevice.Device{"fan1": fanWithTemp()}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	fm := withMetrics(t, s, now)
	fm.points = series(now, 1, 2)

	require.Equal(t, 200, get(s, "/devices/fan1/sparklines?range=7d").Code)
	require.Equal(t, 200, get(s, "/devices/fan1/sparklines?range=30d").Code)
	reqs := fm.requests()
	require.Len(t, reqs, 2)
	assert.Equal(t, now.Add(-7*24*time.Hour), reqs[0].GetStart().AsTime())
	assert.Equal(t, now.Add(-30*24*time.Hour), reqs[1].GetStart().AsTime())

	assert.Equal(t, 400, get(s, "/devices/fan1/sparklines?range=1y").Code)
	assert.Len(t, fm.requests(), 2, "a bad range must not reach metricsd")
}

func TestSparklinesNotFoundWithoutMetricsServer(t *testing.T) {
	s, _, _ := startTestServer(t)
	assert.Equal(t, 404, get(s, "/devices/fan1/sparklines").Code)
}

// The metric to read comes from the device, not the request: a client can't
// ask for an arbitrary series by passing one.
func TestSparklinesIgnoreClientSuppliedMetric(t *testing.T) {
	s, _, bridge := startTestServer(t)
	bridge.devices = map[string]*apiDevice.Device{"fan1": fanWithTemp()}
	fm := withMetrics(t, s, time.Now())
	fm.points = series(time.Now(), 1, 2)

	get(s, "/devices/fan1/sparklines?metric=on_off.is_on&attribute=power.power_w")
	reqs := fm.requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "temperature.value", reqs[0].GetAttribute())
}

func TestSparklinesAreCachedForAMinute(t *testing.T) {
	s, _, bridge := startTestServer(t)
	bridge.devices = map[string]*apiDevice.Device{"fan1": fanWithTemp()}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	fm := withMetrics(t, s, now)
	fm.points = series(now, 1, 2)

	get(s, "/devices/fan1/sparklines")
	get(s, "/devices/fan1/sparklines")
	assert.Len(t, fm.requests(), 1, "second view inside the TTL is served from cache")

	// A different range is a different entry.
	get(s, "/devices/fan1/sparklines?range=7d")
	assert.Len(t, fm.requests(), 2)

	s.metrics.now = func() time.Time { return now.Add(sparkCacheTTL + time.Second) }
	get(s, "/devices/fan1/sparklines")
	assert.Len(t, fm.requests(), 3, "an expired entry is refetched")
}

func TestSparklinesFailureShowsNoteAndIsNotCached(t *testing.T) {
	s, _, bridge := startTestServer(t)
	bridge.devices = map[string]*apiDevice.Device{"fan1": fanWithTemp()}
	fm := withMetrics(t, s, time.Now())
	fm.err = status.Error(codes.Unavailable, "influx down: secret detail")

	rec := get(s, "/devices/fan1/sparklines")
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), "history unavailable")
	assert.NotContains(t, rec.Body.String(), "secret detail")
	assert.NotContains(t, rec.Body.String(), "<polyline")

	// Recovery shows up on the next view, not after the TTL.
	fm.mu.Lock()
	fm.err = nil
	fm.points = series(time.Now(), 1, 2)
	fm.mu.Unlock()
	assert.Contains(t, get(s, "/devices/fan1/sparklines").Body.String(), "<polyline")
}

func TestSparklinesDoNotRideTheLivePush(t *testing.T) {
	dv := deviceToView(fanWithTemp())
	dv.Sparks, dv.SparksHref = true, sparkHref(dv.ID)
	for _, name := range []string{"device_row_oob", "device_readings_oob"} {
		var sb strings.Builder
		require.NoError(t, fragments.ExecuteTemplate(&sb, name, dv))
		assert.NotContains(t, sb.String(), "sparks", name)
	}
}

func TestBuildSpark(t *testing.T) {
	start := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	rd := houseview.Reading{Label: "Temp", Value: "21.0°C", Metric: "temperature.value"}
	pt := func(at time.Time, v float64) *api2.SeriesPoint {
		return &api2.SeriesPoint{Time: timestamppb.New(at), Value: v}
	}

	t.Run("scales time over the whole window and flips y", func(t *testing.T) {
		sv := buildSpark(rd, []*api2.SeriesPoint{pt(start.Add(12*time.Hour), 10), pt(end, 20)}, start, end)
		assert.Empty(t, sv.Note)
		// Data starting at noon sits in the right half; the max is at the top
		// (smallest y), the min at the bottom.
		assert.Equal(t, "60.0,22.0 120.0,2.0", sv.Points)
		assert.Equal(t, "10", sv.Min)
		assert.Equal(t, "20", sv.Max)
	})

	t.Run("a flat series is a centred line", func(t *testing.T) {
		sv := buildSpark(rd, []*api2.SeriesPoint{pt(start, 5), pt(end, 5)}, start, end)
		assert.Equal(t, "0.0,12.0 120.0,12.0", sv.Points)
	})

	t.Run("points outside the window are clamped", func(t *testing.T) {
		sv := buildSpark(rd, []*api2.SeriesPoint{pt(start.Add(-time.Hour), 1), pt(end.Add(time.Hour), 2)}, start, end)
		assert.Equal(t, "0.0,22.0 120.0,2.0", sv.Points)
	})

	t.Run("under two points is a note, not a chart", func(t *testing.T) {
		for _, pts := range [][]*api2.SeriesPoint{nil, {pt(start, 1)}} {
			sv := buildSpark(rd, pts, start, end)
			assert.Equal(t, "no history yet", sv.Note)
			assert.Empty(t, sv.Points)
		}
	})
}
