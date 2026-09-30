package houseview

import (
	"testing"

	"github.com/stretchr/testify/assert"

	api2 "github.com/rmrobinson/house/api"
)

func TestPropertiesToView(t *testing.T) {
	// nil Properties (no linked Sensor has reported anything yet) is every
	// field "" - room.html shows that as "Unknown".
	assert.Equal(t, Properties{}, PropertiesToView(nil))

	occupied := true
	tempF := 71.6
	lux := int32(150)
	aqi := int32(42)
	powerW := 12.3
	pv := PropertiesToView(&api2.Room_Properties{
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
	pv = PropertiesToView(&api2.Room_Properties{Occupied: &notOccupied})
	assert.Equal(t, "No", pv.Occupied)
	assert.Equal(t, "", pv.TemperatureF)
}
