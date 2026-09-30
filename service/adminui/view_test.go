package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	api2 "github.com/rmrobinson/house/api"
)

func TestStrategyStrRoundTrip(t *testing.T) {
	strategies := []api2.AggregationConfig_Strategy{
		api2.AggregationConfig_STRATEGY_UNSPECIFIED,
		api2.AggregationConfig_LATEST,
		api2.AggregationConfig_AVERAGE,
		api2.AggregationConfig_MIN,
		api2.AggregationConfig_MAX,
		api2.AggregationConfig_SUM,
		api2.AggregationConfig_ANY,
	}
	for _, s := range strategies {
		assert.Equal(t, s, strategyFromStr(strategyToStr(s)))
	}

	// An unrecognized string (shouldn't arrive short of a hand-crafted
	// request) falls back to unspecified rather than erroring.
	assert.Equal(t, api2.AggregationConfig_STRATEGY_UNSPECIFIED, strategyFromStr("not-a-strategy"))
}

func TestAggregationToView(t *testing.T) {
	// A nil Aggregation (no override ever configured) renders every field
	// as "unspecified" - room.html shows that as "Default (...)".
	assert.Equal(t, aggregationView{
		OccupancyStrategy:   "unspecified",
		TemperatureStrategy: "unspecified",
		LightStrategy:       "unspecified",
		AirQualityStrategy:  "unspecified",
		PowerStrategy:       "unspecified",
	}, aggregationToView(nil))

	assert.Equal(t, aggregationView{
		OccupancyStrategy:   "any",
		TemperatureStrategy: "average",
		LightStrategy:       "max",
		AirQualityStrategy:  "min",
		PowerStrategy:       "sum",
	}, aggregationToView(&api2.AggregationConfig{
		OccupancyStrategy:   api2.AggregationConfig_ANY,
		TemperatureStrategy: api2.AggregationConfig_AVERAGE,
		LightStrategy:       api2.AggregationConfig_MAX,
		AirQualityStrategy:  api2.AggregationConfig_MIN,
		PowerStrategy:       api2.AggregationConfig_SUM,
	}))
}

func TestPropertiesToView(t *testing.T) {
	// nil Properties (no linked Sensor has reported anything yet) is every
	// field "" - room.html shows that as "Unknown".
	assert.Equal(t, propertiesView{}, propertiesToView(nil))

	occupied := true
	tempF := 71.6
	lux := int32(150)
	aqi := int32(42)
	powerW := 12.3
	pv := propertiesToView(&api2.Room_Properties{
		Occupied:        &occupied,
		TemperatureF:    &tempF,
		LightLevelLux:   &lux,
		AirQualityIndex: &aqi,
		PowerDrawW:      &powerW,
	})
	assert.Equal(t, "Yes", pv.Occupied)
	assert.Equal(t, "71.6°F", pv.TemperatureF)
	assert.Equal(t, "150 lux", pv.LightLevelLux)
	assert.Equal(t, "42", pv.AirQualityIndex)
	assert.Equal(t, "12.3 W", pv.PowerDrawW)

	// A metric with no sensor contributing to it - False is distinguished
	// from "unset" the same way, but only via the pointer itself.
	notOccupied := false
	pv = propertiesToView(&api2.Room_Properties{Occupied: &notOccupied})
	assert.Equal(t, "No", pv.Occupied)
	assert.Equal(t, "", pv.TemperatureF)
}

func TestRoomToView_IncludesAggregationAndProperties(t *testing.T) {
	occupied := true
	room := &api2.Room{
		Id: "room-1",
		Config: &api2.Room_Config{
			Name: "Kitchen",
			Aggregation: &api2.AggregationConfig{
				PowerStrategy: api2.AggregationConfig_MAX,
			},
		},
		Properties: &api2.Room_Properties{Occupied: &occupied},
	}

	rv := roomToView(room)
	assert.Equal(t, "max", rv.Aggregation.PowerStrategy)
	assert.Equal(t, "unspecified", rv.Aggregation.OccupancyStrategy)
	assert.Equal(t, "Yes", rv.Properties.Occupied)
}
