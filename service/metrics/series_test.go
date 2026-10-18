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
	assert.Contains(t, flux, `r.device_id == "dev$1"`, "a bare $ is a legal literal character")
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

// A device id is caller-supplied, so it must not be able to leave its string
// literal or start an interpolation.
func TestFluxStringEscapes(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:             `"plain"`,
		`dev$1`:             `"dev$1"`,
		`say "hi"`:          `"say \"hi\""`,
		`back\slash`:        `"back\\slash"`,
		"two\nlines":        `"two\nlines"`,
		`${secret}`:         `"\${secret}"`,
		`x${a}${b}`:         `"x\${a}\${b}"`,
		`"; drop() //`:      `"\"; drop() //"`,
		`\${x}`:             `"\\\${x}"`,
		`end\`:              `"end\\"`,
		`"}) |> yield() //`: `"\"}) |> yield() //"`,
	} {
		assert.Equal(t, want, fluxString(in), in)
	}
}

func TestSeriesMultiDeviceQuery(t *testing.T) {
	now := time.Now()
	var flux string
	q := newSeriesQuerier(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		flux = string(b)
		_, _ = w.Write([]byte(seriesCSV))
	})

	for combine, fn := range map[Combine]string{CombineMean: "mean", CombineMax: "max", CombineSum: "sum"} {
		res, err := q.Series(context.Background(), SeriesQuery{
			DeviceIDs: []string{"a", "b$"}, Combine: combine, Trait: "power", Field: "power_w",
			Start: now.Add(-24 * time.Hour), End: now, MaxPoints: 48,
		})
		require.NoError(t, err)
		assert.Len(t, res.Points, 2)

		// Each device is windowed on its own, then merged across devices
		// per window by the chosen function.
		assert.Contains(t, flux, `contains(value: r.device_id, set: ["a", "b$"])`)
		assert.NotContains(t, flux, `r.device_id ==`)
		assert.Contains(t, flux, `group(columns: ["device_id"])`)
		assert.Contains(t, flux, `group(columns: ["_time"])`)
		assert.Contains(t, flux, "|> "+fn+"()")
		win, merge := strings.Index(flux, "aggregateWindow"), strings.Index(flux, `group(columns: ["_time"])`)
		require.GreaterOrEqual(t, win, 0, "the per-device window step must be present")
		require.GreaterOrEqual(t, merge, 0, "the cross-device merge step must be present")
		assert.Less(t, win, merge, "windowing must precede the cross-device merge")
		assert.Less(t, merge, strings.Index(flux, "|> "+fn+"()"), "the merge function runs after regrouping by time")
	}
}

// The single-device query is the one verified against live data; adding the
// multi-device path must not change it.
func TestSeriesSingleDeviceQueryIsUnchangedByMultiSupport(t *testing.T) {
	sq := SeriesQuery{
		DeviceID: "dev", Trait: "air_quality", Field: "co2_ppm",
		Start: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC),
	}
	want := `from(bucket: "house")
  |> range(start: 2026-10-17T00:00:00Z, stop: 2026-10-18T00:00:00Z)
  |> filter(fn: (r) => r._measurement == "state" and r.device_id == "dev" and r.trait == "air_quality" and r._field == "co2_ppm")
  |> toFloat()
  |> group()
  |> sort(columns: ["_time"])
  |> aggregateWindow(every: 1800s, fn: mean, createEmpty: false, timeSrc: "_start")
  |> keep(columns: ["_time", "_value"])`
	assert.Equal(t, want, seriesFlux("house", "", sq, 30*time.Minute))
}

func TestSeriesMultiDeviceRollupFallsBackToLast(t *testing.T) {
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
		DeviceIDs: []string{"a", "b"}, Combine: CombineMax, Trait: "water", Field: "is_active",
		Start: now.Add(-60 * 24 * time.Hour), End: now, MaxPoints: 50,
	})
	require.NoError(t, err)
	assert.Len(t, res.Points, 2)
	require.Len(t, queries, 2)
	assert.Contains(t, queries[0], "contains(value: r.device_id")
	assert.Contains(t, queries[1], `r.agg == "last"`)
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

	t.Run("device_ids are de-duplicated and carry the combine mode", func(t *testing.T) {
		f := &fakeSeries{}
		_, err := newServer(f).GetSeries(context.Background(), &api2.GetSeriesRequest{
			DeviceIds: []string{"a", "b", "a", "c"}, Attribute: "power.power_w", Start: start,
			Combine: api2.GetSeriesRequest_COMBINE_SUM,
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "b", "c"}, f.got.DeviceIDs)
		assert.Empty(t, f.got.DeviceID)
		assert.Equal(t, CombineSum, f.got.Combine)
	})

	t.Run("combine modes map, unspecified is the mean", func(t *testing.T) {
		for in, want := range map[api2.GetSeriesRequest_Combine]Combine{
			api2.GetSeriesRequest_COMBINE_UNSPECIFIED: CombineMean,
			api2.GetSeriesRequest_COMBINE_MEAN:        CombineMean,
			api2.GetSeriesRequest_COMBINE_MAX:         CombineMax,
			api2.GetSeriesRequest_COMBINE_SUM:         CombineSum,
		} {
			f := &fakeSeries{}
			_, err := newServer(f).GetSeries(context.Background(), &api2.GetSeriesRequest{
				DeviceIds: []string{"a", "b"}, Attribute: "a.b", Start: start, Combine: in,
			})
			require.NoError(t, err)
			assert.Equal(t, want, f.got.Combine, in.String())
		}
	})

	t.Run("one distinct device in device_ids takes the single-device query", func(t *testing.T) {
		f := &fakeSeries{}
		_, err := newServer(f).GetSeries(context.Background(), &api2.GetSeriesRequest{
			DeviceIds: []string{"only", "only"}, Attribute: "a.b", Start: start,
		})
		require.NoError(t, err)
		assert.Equal(t, "only", f.got.DeviceID)
		assert.Empty(t, f.got.DeviceIDs)
	})

	t.Run("bad device selection", func(t *testing.T) {
		tooMany := make([]string, MaxSeriesDevices+1)
		for i := range tooMany {
			tooMany[i] = "d" + strconv.Itoa(i)
		}
		for name, req := range map[string]*api2.GetSeriesRequest{
			"both device_id and device_ids": {DeviceId: "a", DeviceIds: []string{"b", "c"}, Attribute: "a.b", Start: start},
			"empty id in device_ids":        {DeviceIds: []string{"a", ""}, Attribute: "a.b", Start: start},
			"too many devices":              {DeviceIds: tooMany, Attribute: "a.b", Start: start},
			"unknown combine":               {DeviceIds: []string{"a", "b"}, Attribute: "a.b", Start: start, Combine: 99},
		} {
			t.Run(name, func(t *testing.T) {
				_, err := newServer(&fakeSeries{}).GetSeries(context.Background(), req)
				assert.Equal(t, codes.InvalidArgument, status.Code(err))
			})
		}

		// Exactly the cap is allowed.
		_, err := newServer(&fakeSeries{}).GetSeries(context.Background(), &api2.GetSeriesRequest{
			DeviceIds: tooMany[:MaxSeriesDevices], Attribute: "a.b", Start: start,
		})
		assert.NoError(t, err)

		// The cap counts distinct devices: a longer list that dedupes to
		// within it is fine.
		f := &fakeSeries{}
		dup := append(append([]string{}, tooMany[:MaxSeriesDevices]...), tooMany[:10]...)
		_, err = newServer(f).GetSeries(context.Background(), &api2.GetSeriesRequest{
			DeviceIds: dup, Attribute: "a.b", Start: start,
		})
		require.NoError(t, err)
		assert.Len(t, f.got.DeviceIDs, MaxSeriesDevices)

		// An over-long id is refused for either field, not sent on to Influx.
		long := strings.Repeat("x", MaxDeviceIDLen+1)
		for name, req := range map[string]*api2.GetSeriesRequest{
			"device_id":  {DeviceId: long, Attribute: "a.b", Start: start},
			"device_ids": {DeviceIds: []string{"a", long}, Attribute: "a.b", Start: start},
		} {
			_, err := newServer(&fakeSeries{}).GetSeries(context.Background(), req)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), name)
		}
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
