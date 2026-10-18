package main

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/lib/houseview"
)

// The overview's TRENDS box charts whole-house history: interior climate, house
// power and each EV charger. Unlike the tiles these aren't read from housed's
// room aggregation (it keeps no history); they're rebuilt from the member
// devices' recorded series, merged by metricsd (GetSeries device_ids +
// combine). So a chart approximates its tile - it follows the same devices but
// not housed's per-room strategies - and the box says so.

// Attribute keys as metricsd records them (see houseview.Reading.Metric).
const (
	metricTemp     = "air_properties.temperature_c"
	metricHumidity = "air_properties.humidity_percentage"
	metricCO2      = "air_quality.co2_ppm"
	metricVOC      = "air_quality.volatile_organic_compounds_ppb"
	metricRadon    = "air_quality.radon_bq_m3"

	metricPower       = "power.power_w"
	metricWallPower   = "wall_power.power_w"
	metricSensorPower = "power_consumption.power_usage_w"
)

// maxTrendDevices is metricsd's per-request device cap (MaxSeriesDevices).
const maxTrendDevices = 64

// trendGroup is one GetSeries call: attribute across devices.
type trendGroup struct {
	Attribute string
	DeviceIDs []string
}

// trendSpec is one chart. Several groups (different attributes, or more devices
// than one request allows) are merged point by point with Combine.
type trendSpec struct {
	Label   string
	Unit    string
	Hint    string
	Combine api2.GetSeriesRequest_Combine
	Groups  []trendGroup
}

// trendPlan decides which charts the overview shows and which devices feed
// each, from the snapshot alone. Charts with no member device are omitted.
func trendPlan(snap houseSnapshot) []trendSpec {
	rooms := snap.deviceRooms()
	devices := houseview.SortDevices(snap.allDevices())

	// metrics[id] is the set of series each device can be recorded under.
	metrics := make(map[string]map[string]bool, len(devices))
	for _, d := range devices {
		metrics[d.GetId()] = recordedMetrics(d)
	}
	with := func(metric string, interior bool) []string {
		var ids []string
		for _, d := range devices {
			if interior && rooms[d.GetId()].Exterior {
				continue
			}
			if metrics[d.GetId()][metric] {
				ids = append(ids, d.GetId())
			}
		}
		return ids
	}

	var out []trendSpec
	// Interior climate: temperature and humidity average over the interior
	// sensors; the air-quality charts follow the worst-room tiles, so take the
	// maximum. MEAN can't be split across requests, so it's capped.
	for _, c := range []struct {
		label, unit, hint, metric string
		combine                   api2.GetSeriesRequest_Combine
	}{
		{"Temp", "°C", "interior average", metricTemp, api2.GetSeriesRequest_COMBINE_MEAN},
		{"Humidity", "%", "interior average", metricHumidity, api2.GetSeriesRequest_COMBINE_MEAN},
		{"CO₂", "ppm", "worst sensor", metricCO2, api2.GetSeriesRequest_COMBINE_MAX},
		{"VOC", "ppb", "worst sensor", metricVOC, api2.GetSeriesRequest_COMBINE_MAX},
		{"Radon", "Bq/m³", "worst sensor", metricRadon, api2.GetSeriesRequest_COMBINE_MAX},
	} {
		ids := with(c.metric, true)
		if len(ids) == 0 {
			continue
		}
		if c.combine == api2.GetSeriesRequest_COMBINE_MEAN && len(ids) > maxTrendDevices {
			ids = ids[:maxTrendDevices]
		}
		out = append(out, trendSpec{
			Label: c.label, Unit: c.unit, Hint: c.hint, Combine: c.combine,
			Groups: chunkGroups(c.metric, ids),
		})
	}

	// House power: every metered device, whichever trait carries the watts.
	// Each attribute is its own request; the series are added per window.
	var power []trendGroup
	for _, m := range []string{metricPower, metricWallPower, metricSensorPower} {
		power = append(power, chunkGroups(m, with(m, false))...)
	}
	if len(power) > 0 {
		out = append(out, trendSpec{Label: "House power", Unit: "W", Hint: "metered devices", Combine: api2.GetSeriesRequest_COMBINE_SUM, Groups: power})
	}

	for _, d := range devices {
		if d.GetEvCharger() == nil || !metrics[d.GetId()][metricWallPower] {
			continue
		}
		out = append(out, trendSpec{
			Label: houseview.DisplayName(d), Unit: "W", Hint: "EV charger", Combine: api2.GetSeriesRequest_COMBINE_MEAN,
			Groups: []trendGroup{{Attribute: metricWallPower, DeviceIDs: []string{d.GetId()}}},
		})
	}
	return out
}

// trendTraits maps a power trait's field name to the series it records.
var trendTraits = map[string][]string{
	"power":      {metricPower},
	"wall_power": {metricWallPower},
}

// recordedMetrics is the set of trend series d is recorded under. Climate
// series come from the device's current readings, where a zero means "not
// measured" (houseview.Readings drops it), so a device that reports no radon
// isn't offered for the radon chart. Power series come from which power traits
// the device has instead: there a zero is a real value (an idle charger, a
// switched-off plug) and the device still belongs in a chart of the last 24
// hours.
func recordedMetrics(d *apiDevice.Device) map[string]bool {
	set := map[string]bool{}
	for _, rd := range houseview.Readings(d) {
		if rd.Metric != "" {
			set[rd.Metric] = true
		}
	}
	details := d.ProtoReflect().Descriptor().Oneofs().ByName("details")
	if details == nil {
		return set
	}
	fd := d.ProtoReflect().WhichOneof(details)
	if fd == nil || fd.Message() == nil {
		return set
	}
	dm := d.ProtoReflect().Get(fd).Message()
	for name, ms := range trendTraits {
		f := dm.Descriptor().Fields().ByName(protoreflect.Name(name))
		if f == nil || f.Message() == nil || f.IsList() || !dm.Has(f) {
			continue
		}
		for _, m := range ms {
			set[m] = true
		}
	}
	// A Sensor's PowerConsumption isn't a trait, so it has no entry above; like
	// housed's aggregation, it only counts when the sensor has no Power trait.
	if s := d.GetSensor(); s != nil && s.GetPower() == nil && s.GetPowerConsumption() != nil {
		set[metricSensorPower] = true
	}
	return set
}

// chunkGroups splits ids into requests of at most maxTrendDevices; nil for none.
func chunkGroups(attribute string, ids []string) []trendGroup {
	var out []trendGroup
	for chunk := range slices.Chunk(ids, maxTrendDevices) {
		out = append(out, trendGroup{Attribute: attribute, DeviceIDs: chunk})
	}
	return out
}

// mergeSeries combines series that share window starts: SUM adds and MAX takes
// the larger. A window one series lacks contributes nothing, so a SUM can
// understate where a group has a gap. MEAN is only ever one group, so it
// passes through. Inputs aren't modified (they may be cached).
func mergeSeries(parts [][]*api2.SeriesPoint, combine api2.GetSeriesRequest_Combine) []*api2.SeriesPoint {
	if len(parts) == 1 {
		return parts[0]
	}
	byTime := map[int64]float64{}
	seen := map[int64]bool{}
	for _, part := range parts {
		for _, p := range part {
			k := p.GetTime().AsTime().UnixNano()
			switch {
			case !seen[k]:
				byTime[k] = p.GetValue()
			case combine == api2.GetSeriesRequest_COMBINE_MAX:
				byTime[k] = max(byTime[k], p.GetValue())
			default:
				byTime[k] += p.GetValue()
			}
			seen[k] = true
		}
	}
	keys := make([]int64, 0, len(byTime))
	for k := range byTime {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]*api2.SeriesPoint, len(keys))
	for i, k := range keys {
		out[i] = &api2.SeriesPoint{Time: timestamppb.New(time.Unix(0, k)), Value: byTime[k]}
	}
	return out
}

type trendView struct {
	sparkView
	Unit, Hint string
}

type trendsView struct {
	Ranges []rangeLink
	Charts []trendView
}

func trendsHref(buildingID string) string {
	return "/buildings/" + url.PathEscape(buildingID) + "/overview/trends"
}

// trendCharts fetches and lays out every spec over rng. A chart whose
// history can't be read shows a note instead; a partial sum would mislead.
func (s *Server) trendCharts(ctx context.Context, specs []trendSpec, rng sparkRange) []trendView {
	end := s.metrics.now()
	start := end.Add(-rng.Dur)
	charts := make([]trendView, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tv := trendView{Unit: spec.Unit, Hint: spec.Hint}
			tv.sparkView = sparkView{Label: spec.Label}

			parts := make([][]*api2.SeriesPoint, len(spec.Groups))
			errs := make([]error, len(spec.Groups))
			var gwg sync.WaitGroup
			for gi, g := range spec.Groups {
				gwg.Add(1)
				go func() {
					defer gwg.Done()
					parts[gi], errs[gi] = s.metrics.combined(ctx, g.DeviceIDs, g.Attribute, spec.Combine, rng)
				}()
			}
			gwg.Wait()
			for gi, err := range errs {
				if err != nil {
					s.logger.Warn("unable to read trend history", zap.String("chart", spec.Label),
						zap.String("metric", spec.Groups[gi].Attribute), zap.Error(err))
					tv.Note = "history unavailable"
					charts[i] = tv
					return
				}
			}

			pts := mergeSeries(parts, spec.Combine)
			tv.sparkView = buildSpark(houseview.Reading{Label: spec.Label}, pts, start, end)
			if len(pts) > 0 {
				tv.Value = sparkNumber(pts[len(pts)-1].GetValue())
			}
			charts[i] = tv
		}()
	}
	wg.Wait()
	return charts
}

// handleTrends renders the overview's TRENDS box over ?range= (24h, 7d or
// 30d; default 24h). Which devices and metrics to chart come from the live
// snapshot, never from the request.
func (s *Server) handleTrends(w http.ResponseWriter, r *http.Request) {
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
	snap, err := s.loadSnapshot(r.Context(), id)
	if err != nil {
		s.httpError(w, r, err)
		return
	}

	links := make([]rangeLink, 0, len(sparkRanges))
	for _, sr := range sparkRanges {
		links = append(links, rangeLink{Key: sr.Key, Href: trendsHref(id) + "?range=" + sr.Key, Active: sr.Key == rng.Key})
	}
	s.renderFragment(w, "overview_trends", trendsView{
		Ranges: links,
		Charts: s.trendCharts(r.Context(), trendPlan(snap), rng),
	})
}
