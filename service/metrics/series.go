package metrics

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultRollupBucket is the hourly long-term bucket created by
	// scripts/setup-metrics-influx.sh.
	DefaultRollupBucket = "house_1h"

	// RollupAfter is how far back a range must start before it is read from
	// the rollup bucket instead of the raw one. It is the same cutoff the
	// Grafana dashboard uses, a little under the raw bucket's 30d retention
	// so a range never straddles the point where raw data runs out.
	RollupAfter = 28 * 24 * time.Hour

	// rollupWindow is the rollup bucket's resolution; asking for finer is
	// meaningless.
	rollupWindow = time.Hour
	// rawMinWindow floors raw-bucket windows: the sampler's default heartbeat
	// is 5m, so finer windows would mostly be empty.
	rawMinWindow = time.Minute
)

// SeriesQuery selects one attribute's values over [Start, End).
type SeriesQuery struct {
	DeviceID string
	// Trait and Field are the "trait" tag and the Influx field key.
	Trait, Field string
	Start, End   time.Time
	// MaxPoints bounds the result; the window is widened to fit.
	MaxPoints int
}

// SeriesPoint is one windowed mean.
type SeriesPoint struct {
	Time  time.Time
	Value float64
}

// SeriesResult is the answer to a SeriesQuery.
type SeriesResult struct {
	Points []SeriesPoint
	// Window is the width each point averages over.
	Window time.Duration
}

// SeriesQuerier reads recorded attribute history.
type SeriesQuerier interface {
	Series(ctx context.Context, q SeriesQuery) (SeriesResult, error)
}

// seriesWindow picks the aggregation window for q: wide enough that the range
// yields at most MaxPoints points, never finer than the chosen bucket's own
// resolution, and a whole number of seconds (Flux durations are written in
// seconds below).
func seriesWindow(q SeriesQuery, rollup bool) time.Duration {
	floor := rawMinWindow
	if rollup {
		floor = rollupWindow
	}
	w := q.End.Sub(q.Start) / time.Duration(seriesMaxPoints(q))
	if w < floor {
		w = floor
	}
	if rem := w % time.Second; rem != 0 {
		w += time.Second - rem
	}
	if rollup {
		// Keep rollup windows on whole hours so they line up with the stored
		// hourly points rather than straddling two of them.
		if rem := w % rollupWindow; rem != 0 {
			w += rollupWindow - rem
		}
	}
	return w
}

// seriesMaxPoints is q.MaxPoints, defaulting a non-positive value.
func seriesMaxPoints(q SeriesQuery) int {
	if q.MaxPoints <= 0 {
		return DefaultMaxPoints
	}
	return q.MaxPoints
}

// Series implements SeriesQuerier.
//
// The raw bucket is used unless the range starts more than RollupAfter ago.
// The rollup stores each field several times (one series per aggregate,
// tagged "agg"), so only one aggregate is read from it: the hourly mean or,
// for a bool leaf (which has no mean), the last value of the hour. Values are
// converted to float up front, which both lets a bool leaf chart as 0/1 and
// avoids the schema collision that mixed value types hit at group().
//
// Points are stamped at the start of their window in both buckets so a chart
// can stitch them together, and at most MaxPoints are returned.
func (q *InfluxQuerier) Series(ctx context.Context, sq SeriesQuery) (SeriesResult, error) {
	rollup := time.Since(sq.Start) > RollupAfter
	window := seriesWindow(sq, rollup)

	var points []SeriesPoint
	var err error
	if rollup {
		// Numeric leaves carry both mean and last, so asking for both at once
		// would double them up; try mean first and fall back for bools.
		for _, agg := range []string{"mean", "last"} {
			points, err = q.querySeries(ctx, q.rollupBucket, agg, sq, window)
			if err != nil {
				return SeriesResult{}, err
			}
			if len(points) > 0 {
				break
			}
		}
	} else {
		points, err = q.querySeries(ctx, q.bucket, "", sq, window)
		if err != nil {
			return SeriesResult{}, err
		}
	}

	// Windows align to the epoch rather than to Start, so a range can span one
	// more window than it was sized for; drop the oldest to honour the cap.
	if max := seriesMaxPoints(sq); len(points) > max {
		points = points[len(points)-max:]
	}
	return SeriesResult{Points: points, Window: window}, nil
}

// querySeries runs the windowed-mean query against bucket. agg, when set,
// selects one aggregate of a rollup bucket.
func (q *InfluxQuerier) querySeries(ctx context.Context, bucket, agg string, sq SeriesQuery, window time.Duration) ([]SeriesPoint, error) {
	aggFilter := ""
	if agg != "" {
		aggFilter = " and r.agg == " + fluxString(agg)
	}

	flux := fmt.Sprintf(`from(bucket: %s)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == %s and r.device_id == %s and r.trait == %s and r._field == %s%s)
  |> toFloat()
  |> group()
  |> sort(columns: ["_time"])
  |> aggregateWindow(every: %ds, fn: mean, createEmpty: false, timeSrc: "_start")
  |> keep(columns: ["_time", "_value"])`,
		fluxString(bucket),
		sq.Start.UTC().Format(time.RFC3339), sq.End.UTC().Format(time.RFC3339),
		fluxString(StateMeasurement), fluxString(sq.DeviceID), fluxString(sq.Trait), fluxString(sq.Field),
		aggFilter,
		int64(window/time.Second))

	body, err := q.runFlux(ctx, flux)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return parseSeries(body)
}

// runFlux POSTs flux to the query API and returns the CSV body of a 2xx.
func (q *InfluxQuerier) runFlux(ctx context.Context, flux string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.queryURL, strings.NewReader(flux))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+q.token)
	req.Header.Set("Content-Type", "application/vnd.flux")
	req.Header.Set("Accept", "application/csv")

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("influx query returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}

// parseSeries reads (_time, _value) rows from Influx's annotated CSV,
// returning them in ascending time order. Rows that don't parse are skipped:
// a series with a hole is more useful than none.
func parseSeries(r io.Reader) ([]SeriesPoint, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1

	var out []SeriesPoint
	timeCol, valueCol := -1, -1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse influx csv: %w", err)
		}

		// Each table starts with its own header row.
		if t, v := indexOf(rec, "_time"), indexOf(rec, "_value"); t >= 0 && v >= 0 {
			timeCol, valueCol = t, v
			continue
		}
		if timeCol < 0 || timeCol >= len(rec) || valueCol >= len(rec) {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, rec[timeCol])
		if err != nil {
			continue
		}
		v, err := strconv.ParseFloat(rec[valueCol], 64)
		if err != nil {
			continue
		}
		out = append(out, SeriesPoint{Time: at, Value: v})
	}
	return out, nil
}
