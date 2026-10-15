package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
)

const seriesCSV = `#datatype,string,long,dateTime:RFC3339,double
#group,false,false,false,false
#default,_result,,,
,result,table,_time,_value
,,0,2026-10-14T00:00:00Z,20.5
,,0,2026-10-14T00:05:00Z,21
,,0,not-a-time,9
,,0,2026-10-14T00:10:00Z,notanumber
`

func newSeriesQuerier(t *testing.T, handler http.HandlerFunc) *InfluxQuerier {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	q, err := NewInfluxQuerier(InfluxConfig{
		URL: srv.URL, Org: "falnet", Bucket: "house", RollupBucket: "house_1h", Token: "tok",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)
	return q
}

func TestSeriesRawBucket(t *testing.T) {
	var flux string
	q := newSeriesQuerier(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		flux = string(b)
		assert.Equal(t, "Token tok", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(seriesCSV))
	})

	now := time.Now()
	res, err := q.Series(context.Background(), SeriesQuery{
		DeviceID: "dev$1", Trait: "air_quality", Field: "co2_ppm",
		Start: now.Add(-24 * time.Hour), End: now, MaxPoints: 144,
	})
	require.NoError(t, err)

	// Bad rows are skipped, good ones kept in order.
	require.Len(t, res.Points, 2)
	assert.Equal(t, 20.5, res.Points[0].Value)
	assert.Equal(t, 21.0, res.Points[1].Value)
	// 24h / 144 = 10m.
	assert.Equal(t, 10*time.Minute, res.Window)

	assert.Contains(t, flux, `from(bucket: "house")`)
	assert.NotContains(t, flux, `r.agg`)
	assert.Contains(t, flux, `r.device_id == "dev\$1"`, "device id must be quoted/escaped")
	assert.Contains(t, flux, `r.trait == "air_quality"`)
	assert.Contains(t, flux, `r._field == "co2_ppm"`)
	assert.Contains(t, flux, "every: 600s")
}

func TestSeriesRollupBucket(t *testing.T) {
	var flux string
	q := newSeriesQuerier(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		flux = string(b)
		_, _ = w.Write([]byte(seriesCSV))
	})

	now := time.Now()
	res, err := q.Series(context.Background(), SeriesQuery{
		DeviceID: "d", Trait: "air_properties", Field: "temperature_c",
		Start: now.Add(-30 * 24 * time.Hour), End: now, MaxPoints: 1000,
	})
	require.NoError(t, err)

	assert.Contains(t, flux, `from(bucket: "house_1h")`)
	assert.Contains(t, flux, `r.agg == "mean"`)
	// 30d / 1000 is ~43m, which the hourly rollup can't resolve.
	assert.Equal(t, time.Hour, res.Window)
}

// emptyCSV is what Influx returns for a query that matches nothing.
const emptyCSV = "\r\n"

func TestSeriesRollupFallsBackToLastForBools(t *testing.T) {
	var queries []string
	q := newSeriesQuerier(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		queries = append(queries, string(b))
		if strings.Contains(string(b), `r.agg == "mean"`) {
			_, _ = w.Write([]byte(emptyCSV))
			return
		}
		_, _ = w.Write([]byte(seriesCSV))
	})

	now := time.Now()
	res, err := q.Series(context.Background(), SeriesQuery{
		DeviceID: "d", Trait: "on_off", Field: "is_on",
		Start: now.Add(-60 * 24 * time.Hour), End: now, MaxPoints: 100,
	})
	require.NoError(t, err)
	assert.Len(t, res.Points, 2)
	require.Len(t, queries, 2)
	assert.Contains(t, queries[1], `r.agg == "last"`)
}

func TestSeriesRollupDoesNotRetryWhenMeanHasData(t *testing.T) {
	calls := 0
	q := newSeriesQuerier(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(seriesCSV))
	})
	now := time.Now()
	_, err := q.Series(context.Background(), SeriesQuery{
		DeviceID: "d", Trait: "t", Field: "f",
		Start: now.Add(-60 * 24 * time.Hour), End: now, MaxPoints: 100,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestSeriesStampsWindowStartAndHonoursMaxPoints(t *testing.T) {
	var flux string
	csv := "#datatype,string,long,dateTime:RFC3339,double\n,result,table,_time,_value\n"
	for i := 0; i < 5; i++ {
		csv += ",,0," + time.Date(2026, 10, 14, i, 0, 0, 0, time.UTC).Format(time.RFC3339) + "," + strconv.Itoa(i) + "\n"
	}
	q := newSeriesQuerier(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		flux = string(b)
		_, _ = w.Write([]byte(csv))
	})

	now := time.Now()
	res, err := q.Series(context.Background(), SeriesQuery{
		DeviceID: "d", Trait: "t", Field: "f", Start: now.Add(-time.Hour), End: now, MaxPoints: 3,
	})
	require.NoError(t, err)
	assert.Contains(t, flux, `timeSrc: "_start"`)

	// 5 windows came back for a cap of 3: the newest 3 are kept, in order.
	require.Len(t, res.Points, 3)
	assert.Equal(t, 2.0, res.Points[0].Value)
	assert.Equal(t, 4.0, res.Points[2].Value)
}

func TestSeriesWindowDefaultsZeroMaxPoints(t *testing.T) {
	end := time.Now()
	assert.NotPanics(t, func() {
		got := seriesWindow(SeriesQuery{Start: end.Add(-24 * time.Hour), End: end}, false)
		assert.Equal(t, 24*time.Hour/DefaultMaxPoints, got)
	})
}

func TestSeriesWindow(t *testing.T) {
	end := time.Now()
	for name, tc := range map[string]struct {
		span      time.Duration
		maxPoints int
		rollup    bool
		want      time.Duration
	}{
		"raw floors at a minute":        {time.Hour, 1000, false, time.Minute},
		"raw widens to fit max points":  {24 * time.Hour, 100, false, 864 * time.Second},
		"rollup floors at an hour":      {7 * 24 * time.Hour, 1000, true, time.Hour},
		"rollup rounds up to whole hrs": {400 * 24 * time.Hour, 99, true, 97 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			got := seriesWindow(SeriesQuery{Start: end.Add(-tc.span), End: end, MaxPoints: tc.maxPoints}, tc.rollup)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSeriesInfluxError(t *testing.T) {
	q := newSeriesQuerier(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	_, err := q.Series(context.Background(), SeriesQuery{
		DeviceID: "d", Trait: "t", Field: "f",
		Start: time.Now().Add(-time.Hour), End: time.Now(), MaxPoints: 10,
	})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "401"))
}

type fakeSeries struct {
	got SeriesQuery
	res SeriesResult
	err error
}

func (f *fakeSeries) Series(_ context.Context, q SeriesQuery) (SeriesResult, error) {
	f.got = q
	return f.res, f.err
}

func TestGetSeries(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	newServer := func(f *fakeSeries) *Server {
		s := NewServer(zaptest.NewLogger(t), f)
		s.now = func() time.Time { return now }
		return s
	}
	start := timestamppb.New(now.Add(-24 * time.Hour))

	t.Run("splits attribute at first dot, defaults end and max_points", func(t *testing.T) {
		f := &fakeSeries{res: SeriesResult{
			Points: []SeriesPoint{{Time: now.Add(-time.Hour), Value: 3}},
			Window: 10 * time.Minute,
		}}
		resp, err := newServer(f).GetSeries(context.Background(), &api2.GetSeriesRequest{
			DeviceId: "dev", Attribute: "light.colour.hue", Start: start,
		})
		require.NoError(t, err)

		assert.Equal(t, "light", f.got.Trait)
		assert.Equal(t, "colour.hue", f.got.Field)
		assert.Equal(t, now, f.got.End)
		assert.Equal(t, DefaultMaxPoints, f.got.MaxPoints)
		require.Len(t, resp.Points, 1)
		assert.Equal(t, 3.0, resp.Points[0].Value)
		assert.Equal(t, 10*time.Minute, resp.Resolution.AsDuration())
	})

	t.Run("caps max_points", func(t *testing.T) {
		f := &fakeSeries{}
		_, err := newServer(f).GetSeries(context.Background(), &api2.GetSeriesRequest{
			DeviceId: "dev", Attribute: "a.b", Start: start, MaxPoints: 1_000_000,
		})
		require.NoError(t, err)
		assert.Equal(t, MaxMaxPoints, f.got.MaxPoints)
	})

	t.Run("empty result is not an error", func(t *testing.T) {
		resp, err := newServer(&fakeSeries{}).GetSeries(context.Background(), &api2.GetSeriesRequest{
			DeviceId: "dev", Attribute: "a.b", Start: start,
		})
		require.NoError(t, err)
		assert.Empty(t, resp.Points)
	})

	t.Run("bad input", func(t *testing.T) {
		for name, req := range map[string]*api2.GetSeriesRequest{
			"no device":        {Attribute: "a.b", Start: start},
			"no attribute":     {DeviceId: "d", Start: start},
			"no dot":           {DeviceId: "d", Attribute: "co2_ppm", Start: start},
			"empty leaf":       {DeviceId: "d", Attribute: "air_quality.", Start: start},
			"empty trait":      {DeviceId: "d", Attribute: ".co2_ppm", Start: start},
			"no start":         {DeviceId: "d", Attribute: "a.b"},
			"end before start": {DeviceId: "d", Attribute: "a.b", Start: start, End: timestamppb.New(now.Add(-48 * time.Hour))},
			"range too long":   {DeviceId: "d", Attribute: "a.b", Start: timestamppb.New(now.Add(-400 * 24 * time.Hour))},
			"negative points":  {DeviceId: "d", Attribute: "a.b", Start: start, MaxPoints: -1},
		} {
			t.Run(name, func(t *testing.T) {
				_, err := newServer(&fakeSeries{}).GetSeries(context.Background(), req)
				assert.Equal(t, codes.InvalidArgument, status.Code(err))
			})
		}
	})

	t.Run("store failure is unavailable and doesn't leak detail", func(t *testing.T) {
		_, err := newServer(&fakeSeries{err: errors.New("influx query returned 401: secret detail")}).
			GetSeries(context.Background(), &api2.GetSeriesRequest{DeviceId: "d", Attribute: "a.b", Start: start})
		assert.Equal(t, codes.Unavailable, status.Code(err))
		assert.NotContains(t, err.Error(), "secret detail")
	})

	t.Run("caller cancellation and deadline keep their own codes", func(t *testing.T) {
		for want, cause := range map[codes.Code]error{
			codes.Canceled:         fmt.Errorf("do request: %w", context.Canceled),
			codes.DeadlineExceeded: fmt.Errorf("do request: %w", context.DeadlineExceeded),
		} {
			_, err := newServer(&fakeSeries{err: cause}).
				GetSeries(context.Background(), &api2.GetSeriesRequest{DeviceId: "d", Attribute: "a.b", Start: start})
			assert.Equal(t, want, status.Code(err))
		}
	})
}
