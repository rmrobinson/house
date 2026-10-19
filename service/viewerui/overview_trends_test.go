package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
)

func climateSensor(id string, tempC, humidity float32, co2 int32) *apiDevice.Device {
	return &apiDevice.Device{Id: id, Address: &apiDevice.Device_Address{IsReachable: true}, Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
		AirProperties: &apiTrait.AirProperties{State: &apiTrait.AirProperties_State{TemperatureC: tempC, HumidityPercentage: humidity}},
		AirQuality:    &apiTrait.AirQuality{State: &apiTrait.AirQuality_State{Co2Ppm: &co2}},
	}}}
}

func meteredPlug(id string, watts float64) *apiDevice.Device {
	return &apiDevice.Device{Id: id, Address: &apiDevice.Device_Address{IsReachable: true}, Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
		Power: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: watts}},
	}}}
}

func evCharger(id string, watts float64) *apiDevice.Device {
	return &apiDevice.Device{Id: id, Config: &apiDevice.Device_Config{Name: "Wall Charger"}, Address: &apiDevice.Device_Address{IsReachable: true},
		Details: &apiDevice.Device_EvCharger{EvCharger: &apiDevice.EVCharger{
			WallPower: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: watts}},
		}}}
}

func trendSnap(interior, exterior []*apiDevice.Device, unlinked ...*apiDevice.Device) houseSnapshot {
	return houseSnapshot{
		Building: &api2.Building{Id: "b1", Config: &api2.Building_Config{Name: "Home"}},
		Floors:   []*api2.Floor{{Id: "f1", Name: "Main"}, {Id: "f2", Name: "Property", Exterior: true}},
		Rooms: map[string][]*api2.Room{
			"f1": {{Id: "r1", Config: &api2.Room_Config{Name: "Den"}, Devices: interior}},
			"f2": {{Id: "r2", Config: &api2.Room_Config{Name: "Porch"}, Devices: exterior}},
		},
		Devices: unlinked,
	}
}

func specByLabel(t *testing.T, specs []trendSpec, label string) trendSpec {
	t.Helper()
	for _, s := range specs {
		if s.Label == label {
			return s
		}
	}
	t.Fatalf("no trend %q in %+v", label, specs)
	return trendSpec{}
}

func TestTrendPlanInteriorClimateExcludesExterior(t *testing.T) {
	snap := trendSnap(
		[]*apiDevice.Device{climateSensor("in1", 21, 40, 900), climateSensor("in2", 22, 50, 1100)},
		[]*apiDevice.Device{climateSensor("out1", 5, 90, 400)},
	)
	specs := trendPlan(snap)

	temp := specByLabel(t, specs, "Temp")
	assert.Equal(t, api2.GetSeriesRequest_COMBINE_MEAN, temp.Combine)
	require.Len(t, temp.Groups, 1)
	assert.Equal(t, "air_properties.temperature_c", temp.Groups[0].Attribute)
	assert.Equal(t, []string{"in1", "in2"}, temp.Groups[0].DeviceIDs, "the porch sensor is not interior")

	co2 := specByLabel(t, specs, "CO₂")
	assert.Equal(t, api2.GetSeriesRequest_COMBINE_MAX, co2.Combine, "worst sensor, like the tile")
	assert.Equal(t, "air_quality.co2_ppm", co2.Groups[0].Attribute)
	assert.Equal(t, []string{"in1", "in2"}, co2.Groups[0].DeviceIDs)

	humidity := specByLabel(t, specs, "Humidity")
	assert.Equal(t, api2.GetSeriesRequest_COMBINE_MEAN, humidity.Combine)

	for _, s := range specs {
		assert.NotEqual(t, "VOC", s.Label, "no device reports VOC")
		assert.NotEqual(t, "Radon", s.Label, "no device reports radon")
	}
}

func TestTrendPlanHousePowerSumsEveryMeteredDevice(t *testing.T) {
	sensorPC := &apiDevice.Device{Id: "pc", Address: &apiDevice.Device_Address{IsReachable: true}, Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
		PowerConsumption: &apiDevice.Sensor_PowerConsumption{PowerUsageW: 30},
	}}}
	// A sensor with a Power trait AND PowerConsumption counts once, via the trait.
	both := meteredPlug("both", 10)
	both.GetSensor().PowerConsumption = &apiDevice.Sensor_PowerConsumption{PowerUsageW: 10}

	snap := trendSnap(
		[]*apiDevice.Device{meteredPlug("plug1", 100), both},
		[]*apiDevice.Device{meteredPlug("outdoor", 50)},
		sensorPC, evCharger("ev", 7200),
	)
	power := specByLabel(t, trendPlan(snap), "House power")
	assert.Equal(t, api2.GetSeriesRequest_COMBINE_SUM, power.Combine)

	got := map[string][]string{}
	for _, g := range power.Groups {
		got[g.Attribute] = g.DeviceIDs
	}
	assert.Equal(t, []string{"both", "outdoor", "plug1"}, got["power.power_w"], "exterior devices draw power too")
	assert.Equal(t, []string{"ev"}, got["wall_power.power_w"])
	assert.Equal(t, []string{"pc"}, got["power_consumption.power_usage_w"])
}

func TestTrendPlanEVChargerGetsItsOwnChart(t *testing.T) {
	snap := trendSnap(nil, nil, evCharger("ev", 7200))
	ev := specByLabel(t, trendPlan(snap), "Wall Charger")
	assert.Equal(t, "EV charger", ev.Hint)
	require.Len(t, ev.Groups, 1)
	assert.Equal(t, trendGroup{Attribute: "wall_power.power_w", DeviceIDs: []string{"ev"}}, ev.Groups[0])
}

// An idle charger or a switched-off plug reports 0 W, which houseview.Readings
// omits; it still belongs in the charts, or its past draw would vanish from the
// house-power history.
func TestTrendPlanKeepsDevicesThatAreIdleRightNow(t *testing.T) {
	snap := trendSnap([]*apiDevice.Device{meteredPlug("off", 0)}, nil, evCharger("ev", 0))
	specs := trendPlan(snap)

	ev := specByLabel(t, specs, "Wall Charger")
	assert.Equal(t, []string{"ev"}, ev.Groups[0].DeviceIDs)

	got := map[string][]string{}
	for _, g := range specByLabel(t, specs, "House power").Groups {
		got[g.Attribute] = g.DeviceIDs
	}
	assert.Equal(t, []string{"off"}, got["power.power_w"])
	assert.Equal(t, []string{"ev"}, got["wall_power.power_w"])
}

func TestTrendPlanEmptyHouse(t *testing.T) {
	assert.Empty(t, trendPlan(trendSnap(nil, nil)))
}

func TestTrendPlanSplitsPastTheDeviceCap(t *testing.T) {
	var plugs []*apiDevice.Device
	for i := 0; i < maxTrendDevices+1; i++ {
		plugs = append(plugs, meteredPlug("p"+string(rune('A'+i/26))+string(rune('a'+i%26)), 1))
	}
	power := specByLabel(t, trendPlan(trendSnap(plugs, nil)), "House power")
	require.Len(t, power.Groups, 2, "SUM splits across requests")
	assert.Len(t, power.Groups[0].DeviceIDs, maxTrendDevices)
	assert.Len(t, power.Groups[1].DeviceIDs, 1)

	// MEAN cannot be split, so it is capped instead.
	var sensors []*apiDevice.Device
	for i := 0; i < maxTrendDevices+5; i++ {
		sensors = append(sensors, climateSensor("s"+string(rune('A'+i/26))+string(rune('a'+i%26)), 20, 40, 500))
	}
	temp := specByLabel(t, trendPlan(trendSnap(sensors, nil)), "Temp")
	require.Len(t, temp.Groups, 1)
	assert.Len(t, temp.Groups[0].DeviceIDs, maxTrendDevices)
}

func TestMergeSeries(t *testing.T) {
	base := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	pt := func(h int, v float64) *api2.SeriesPoint {
		return &api2.SeriesPoint{Time: timestamppb.New(base.Add(time.Duration(h) * time.Hour)), Value: v}
	}
	a := []*api2.SeriesPoint{pt(0, 1), pt(1, 2)}
	b := []*api2.SeriesPoint{pt(1, 10), pt(2, 20)}

	sum := mergeSeries([][]*api2.SeriesPoint{a, b}, api2.GetSeriesRequest_COMBINE_SUM)
	require.Len(t, sum, 3)
	assert.Equal(t, []float64{1, 12, 20}, []float64{sum[0].GetValue(), sum[1].GetValue(), sum[2].GetValue()})
	assert.Equal(t, base.Add(2*time.Hour), sum[2].GetTime().AsTime(), "sorted by window start")

	mx := mergeSeries([][]*api2.SeriesPoint{a, b}, api2.GetSeriesRequest_COMBINE_MAX)
	assert.Equal(t, []float64{1, 10, 20}, []float64{mx[0].GetValue(), mx[1].GetValue(), mx[2].GetValue()})

	assert.Equal(t, a, mergeSeries([][]*api2.SeriesPoint{a}, api2.GetSeriesRequest_COMBINE_MEAN), "one part passes through")
	assert.Equal(t, 2, len(a), "inputs (which may be cached) are not modified")
	assert.Equal(t, 1.0, a[0].GetValue())
}

// trendServer is a test server whose room r1 holds two interior sensors and
// whose property floor holds nothing else.
func trendServer(t *testing.T, now time.Time) (*Server, *fakeMetrics) {
	t.Helper()
	s, house, _ := startTestServer(t)
	house.setExtraDevice(climateSensor("in1", 21, 40, 900))
	fm := withMetrics(t, s, now)
	fm.points = series(now, 20, 22, 24)
	return s, fm
}

func TestTrendsRendersChartsAndAsksForTheRightSeries(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, fm := trendServer(t, now)

	rec := get(s, "/buildings/b1/overview/trends", "HX-Request", "true")
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "<polyline")
	assert.Contains(t, body, "Temp")
	assert.Contains(t, body, "20–24")
	assert.Contains(t, body, "not identical", "the box says the charts approximate the tiles")
	for _, r := range []string{"24h", "7d", "30d"} {
		assert.Contains(t, body, `hx-get="/buildings/b1/overview/trends?range=`+r+`"`)
	}
	assert.Contains(t, body, `hx-target="#trends"`)

	var temp *api2.GetSeriesRequest
	for _, r := range fm.requests() {
		if r.GetAttribute() == "air_properties.temperature_c" {
			temp = r
		}
	}
	require.NotNil(t, temp)
	assert.Equal(t, "in1", temp.GetDeviceId(), "a single device uses the plain device_id path")
	assert.Equal(t, api2.GetSeriesRequest_COMBINE_MEAN, temp.GetCombine())
	assert.Equal(t, now.Add(-24*time.Hour), temp.GetStart().AsTime())
	assert.Equal(t, now, temp.GetEnd().AsTime(), "end is pinned so combined windows line up")
}

// One chart is visible at a time: every measurement gets a tab, the first
// starts selected, and the rest are rendered (so hovering is instant) but not
// shown.
func TestSparkSetShowsOneChartWithTheFirstSelected(t *testing.T) {
	charts := []sparkView{
		{Label: "Temp", Value: "21°C", Points: "0,1 2,3", Min: "20", Max: "22"},
		{Label: "CO2", Value: "600 ppm", Points: "0,1 2,3", Min: "500", Max: "700"},
		{Label: "VOC", Note: "no history yet"},
	}
	var sb strings.Builder
	require.NoError(t, fragments.ExecuteTemplate(&sb, "spark_set", charts))
	html := sb.String()

	assert.Equal(t, 3, strings.Count(html, `role="tab"`))
	assert.Equal(t, 3, strings.Count(html, `role="tabpanel"`))
	assert.Equal(t, 1, strings.Count(html, `class="spark-tab on"`), "only the first tab is highlighted")
	assert.Equal(t, 1, strings.Count(html, `class="spark-panel on"`), "only the first chart is shown")
	assert.Contains(t, html, `class="spark-tab on" role="tab" tabindex="0" data-spark="Temp" aria-selected="true"`)
	assert.Contains(t, html, `tabindex="-1" data-spark="VOC" aria-selected="false"`, "only the selected tab is in the tab order")
	assert.Contains(t, html, `class="spark-panel" role="tabpanel" data-spark="CO2"`, "panels are keyed like their tabs")
	assert.Contains(t, html, "no history yet")
}

func TestTrendsAndDeviceSparksUseTheTabbedSet(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, _ := trendServer(t, now)
	body := get(s, "/buildings/b1/overview/trends").Body.String()
	assert.Contains(t, body, `class="spark-tab on"`)
	assert.Equal(t, 1, strings.Count(body, `class="spark-panel on"`))

	s2, _, bridge := startTestServer(t)
	bridge.devices = map[string]*apiDevice.Device{"fan1": fanWithTemp()}
	withMetrics(t, s2, now).points = series(now, 1, 2)
	body = get(s2, "/devices/fan1/sparklines").Body.String()
	assert.Contains(t, body, `class="spark-tab on"`)
}

// A SUM over more devices than one request may carry is split into chunks whose
// per-window values are added back together, and the chart is built from that.
func TestTrendsChunkedSumIsAddedAcrossRequests(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, _, _ := startTestServer(t)
	var plugs []*apiDevice.Device
	for i := 0; i < maxTrendDevices+1; i++ {
		plugs = append(plugs, meteredPlug("p"+string(rune('A'+i/26))+string(rune('a'+i%26)), 1))
	}
	spec := specByLabel(t, trendPlan(trendSnap(plugs, nil)), "House power")
	require.Len(t, spec.Groups, 2)

	fm := withMetrics(t, s, now)
	fm.points = series(now, 100, 200)
	charts := s.trendCharts(t.Context(), []trendSpec{spec}, sparkRanges[0])
	require.Len(t, charts, 1)

	reqs := fm.requests()
	require.Len(t, reqs, 2, "one GetSeries per chunk")
	var sizes []int
	for _, r := range reqs {
		n := len(r.GetDeviceIds())
		if r.GetDeviceId() != "" {
			n = 1
		}
		sizes = append(sizes, n)
	}
	assert.ElementsMatch(t, []int{maxTrendDevices, 1}, sizes)
	for _, r := range reqs {
		assert.Equal(t, api2.GetSeriesRequest_COMBINE_SUM, r.GetCombine())
		assert.Equal(t, "power.power_w", r.GetAttribute())
	}
	// Both chunks returned the same two windows, so each window is doubled.
	assert.Equal(t, "400", charts[0].Value, "latest window: 200 + 200")
	assert.Equal(t, "200", charts[0].Min)
	assert.Equal(t, "400", charts[0].Max)
}

func TestTrendsRangeAndValidation(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, fm := trendServer(t, now)

	require.Equal(t, 200, get(s, "/buildings/b1/overview/trends?range=7d").Code)
	for _, r := range fm.requests() {
		assert.Equal(t, now.Add(-7*24*time.Hour), r.GetStart().AsTime())
	}
	before := len(fm.requests())
	assert.Equal(t, 400, get(s, "/buildings/b1/overview/trends?range=1y").Code)
	assert.Len(t, fm.requests(), before, "a bad range must not reach metricsd")
}

func TestTrendsNotFoundWithoutMetricsServer(t *testing.T) {
	s, _, _ := startTestServer(t)
	assert.Equal(t, 404, get(s, "/buildings/b1/overview/trends").Code)
}

// The client chooses neither devices nor metrics.
func TestTrendsIgnoreClientSuppliedSelection(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, fm := trendServer(t, now)
	get(s, "/buildings/b1/overview/trends?device_ids=x&attribute=on_off.is_on&combine=SUM")
	for _, r := range fm.requests() {
		assert.NotContains(t, r.GetDeviceIds(), "x")
		assert.NotEqual(t, "on_off.is_on", r.GetAttribute())
	}
}

func TestTrendsFailureShowsNoteWithoutLeakingDetail(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, fm := trendServer(t, now)
	fm.err = status.Error(codes.Unavailable, "influx down: secret detail")

	rec := get(s, "/buildings/b1/overview/trends")
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), "history unavailable")
	assert.NotContains(t, rec.Body.String(), "secret detail")
	assert.NotContains(t, rec.Body.String(), "<polyline")
}

func TestTrendsAreCachedForAMinute(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, fm := trendServer(t, now)
	get(s, "/buildings/b1/overview/trends")
	n := len(fm.requests())
	require.NotZero(t, n)
	get(s, "/buildings/b1/overview/trends")
	assert.Len(t, fm.requests(), n)
}

func TestOverviewPageHasTrendsBoxOnlyWithMetricsAndItIsNotPushed(t *testing.T) {
	s, _, _ := startTestServer(t)
	assert.NotContains(t, get(s, "/buildings/b1/overview").Body.String(), `id="trends"`)

	withMetrics(t, s, time.Now())
	page := get(s, "/buildings/b1/overview").Body.String()
	assert.Contains(t, page, `id="trends"`)
	assert.Contains(t, page, `hx-get="/buildings/b1/overview/trends"`)
	assert.Contains(t, page, `hx-trigger="load"`)
	// TRENDS sits between the two pushed regions.
	top, trends, rest := strings.Index(page, `id="overview"`), strings.Index(page, `id="trends"`), strings.Index(page, `id="overview-rest"`)
	assert.True(t, top < trends && trends < rest, "order: %d %d %d", top, trends, rest)

	var sb strings.Builder
	require.NoError(t, fragments.ExecuteTemplate(&sb, "overview_oob", buildOverview(trendSnap(nil, nil))))
	assert.Contains(t, sb.String(), `id="overview" hx-swap-oob`)
	assert.Contains(t, sb.String(), `id="overview-rest" hx-swap-oob`)
	assert.NotContains(t, sb.String(), "trends", "a live re-render must not touch the charts")
}
