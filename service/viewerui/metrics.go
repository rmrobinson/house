package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/houseview"
)

const (
	// sparkPoints is the max_points asked of metricsd: a 120px-wide sparkline
	// can't show more detail than this.
	sparkPoints = 48
	// sparkCacheTTL bounds how stale a chart can be. Opening a room, flipping
	// the range or a second tab inside this window costs no metricsd query.
	sparkCacheTTL = time.Minute
	// sparkTimeout bounds one GetSeries call so a slow metricsd can't hold a
	// room's charts open.
	sparkTimeout = 5 * time.Second
	// sparkMaxCache is the entry count past which expired entries are swept.
	sparkMaxCache = 512

	sparkWidth  = 120.0
	sparkHeight = 24.0
	// sparkPad keeps the stroke inside the viewBox at the extremes.
	sparkPad = 2.0
)

// sparkRange is one selectable chart window.
type sparkRange struct {
	Key string
	Dur time.Duration
}

var sparkRanges = []sparkRange{
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
	{"30d", 30 * 24 * time.Hour},
}

func lookupSparkRange(key string) (sparkRange, bool) {
	if key == "" {
		return sparkRanges[0], true
	}
	for _, r := range sparkRanges {
		if r.Key == key {
			return r, true
		}
	}
	return sparkRange{}, false
}

// metricsFeed reads recorded attribute history from metricsd's MetricsService,
// caching each (device, metric, range) for sparkCacheTTL.
type metricsFeed struct {
	logger *zap.Logger
	client api2.MetricsServiceClient
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cachedSeries
}

type cachedSeries struct {
	points []*api2.SeriesPoint
	at     time.Time
}

func newMetricsFeed(logger *zap.Logger, client api2.MetricsServiceClient) *metricsFeed {
	return &metricsFeed{logger: logger, client: client, now: time.Now, cache: map[string]cachedSeries{}}
}

// series returns metric's history over r. Only successful lookups are cached,
// so a metricsd blip doesn't blank a chart for the TTL.
func (m *metricsFeed) series(ctx context.Context, deviceID, metric string, r sparkRange) ([]*api2.SeriesPoint, error) {
	key := deviceID + "\x00" + metric + "\x00" + r.Key
	now := m.now()

	m.mu.Lock()
	if c, ok := m.cache[key]; ok && now.Sub(c.at) < sparkCacheTTL {
		m.mu.Unlock()
		return c.points, nil
	}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, sparkTimeout)
	defer cancel()
	resp, err := m.client.GetSeries(ctx, &api2.GetSeriesRequest{
		DeviceId:  deviceID,
		Attribute: metric,
		Start:     timestamppb.New(now.Add(-r.Dur)),
		MaxPoints: sparkPoints,
	})
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.cache) >= sparkMaxCache {
		for k, c := range m.cache {
			if now.Sub(c.at) >= sparkCacheTTL {
				delete(m.cache, k)
			}
		}
	}
	m.cache[key] = cachedSeries{points: resp.GetPoints(), at: now}
	return resp.GetPoints(), nil
}

// sparkView is one chart: a reading's label and current value, and the
// polyline for its history (or, when there's nothing to draw, why not).
type sparkView struct {
	Label string
	Value string
	// Points is the SVG polyline "x,y x,y ..." in a sparkWidth x sparkHeight
	// box; empty when Note is set.
	Points   string
	Min, Max string
	Note     string
}

type rangeLink struct {
	Key    string
	Href   string
	Active bool
}

// sparklinesView is the fragment loaded into a device's sparks row.
type sparklinesView struct {
	Ranges []rangeLink
	Charts []sparkView
}

func sparkHref(deviceID string) string {
	return "/devices/" + url.PathEscape(deviceID) + "/sparklines"
}

func rangeLinks(deviceID string, active sparkRange) []rangeLink {
	out := make([]rangeLink, 0, len(sparkRanges))
	for _, r := range sparkRanges {
		out = append(out, rangeLink{Key: r.Key, Href: sparkHref(deviceID) + "?range=" + r.Key, Active: r.Key == active.Key})
	}
	return out
}

// buildSpark lays pts out over [start, end]. Time is scaled to the whole
// window rather than to the data, so a metric that only started being
// recorded recently sits at the right edge instead of stretching to fill it.
func buildSpark(rd houseview.Reading, pts []*api2.SeriesPoint, start, end time.Time) sparkView {
	sv := sparkView{Label: rd.Label, Value: rd.Value}
	if len(pts) < 2 {
		sv.Note = "no history yet"
		return sv
	}

	lo, hi := math.Inf(1), math.Inf(-1)
	for _, p := range pts {
		lo, hi = math.Min(lo, p.GetValue()), math.Max(hi, p.GetValue())
	}
	span := end.Sub(start).Seconds()
	if span <= 0 {
		sv.Note = "no history yet"
		return sv
	}

	var b strings.Builder
	for i, p := range pts {
		x := p.GetTime().AsTime().Sub(start).Seconds() / span * sparkWidth
		x = math.Max(0, math.Min(sparkWidth, x))
		y := sparkHeight / 2 // a flat series draws as a centred line
		if hi > lo {
			y = sparkPad + (hi-p.GetValue())/(hi-lo)*(sparkHeight-2*sparkPad)
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	sv.Points = b.String()
	sv.Min, sv.Max = fmt.Sprintf("%.3g", lo), fmt.Sprintf("%.3g", hi)
	return sv
}

// handleSparklines renders the charts for a device's numeric readings over
// ?range= (24h, 7d or 30d; default 24h). Which metrics to chart is decided here
// from the device itself, never taken from the request, so the route can't be
// used to read arbitrary series.
func (s *Server) handleSparklines(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		http.NotFound(w, r)
		return
	}
	rng, ok := lookupSparkRange(r.URL.Query().Get("range"))
	if !ok {
		http.Error(w, "range must be 24h, 7d or 30d", http.StatusBadRequest)
		return
	}

	id := r.PathValue("id")
	d, err := s.bridge.GetDevice(r.Context(), &api2.GetDeviceRequest{Id: id})
	if err != nil {
		s.httpError(w, r, err)
		return
	}

	var readings []houseview.Reading
	for _, rd := range houseview.Readings(d) {
		if rd.Metric != "" {
			readings = append(readings, rd)
		}
	}

	end := s.metrics.now()
	start := end.Add(-rng.Dur)
	charts := make([]sparkView, len(readings))
	var wg sync.WaitGroup
	for i, rd := range readings {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pts, err := s.metrics.series(r.Context(), id, rd.Metric, rng)
			if err != nil {
				s.logger.Warn("unable to read metric history",
					zap.String("device_id", id), zap.String("metric", rd.Metric), zap.Error(err))
				charts[i] = sparkView{Label: rd.Label, Value: rd.Value, Note: "history unavailable"}
				return
			}
			charts[i] = buildSpark(rd, pts, start, end)
		}()
	}
	wg.Wait()

	s.renderFragment(w, "device_sparklines", sparklinesView{Ranges: rangeLinks(id, rng), Charts: charts})
}

// hasMetricReadings reports whether any of rs can be charted.
func hasMetricReadings(rs []houseview.Reading) bool {
	for _, r := range rs {
		if r.Metric != "" {
			return true
		}
	}
	return false
}
