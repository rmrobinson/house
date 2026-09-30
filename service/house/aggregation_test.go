package house

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/house/db"
)

func floatPtr(f float64) *float64 { return &f }

func TestAggregateNumeric(t *testing.T) {
	samples := []numericSample{{value: 10}, {value: 20}, {value: 30}}

	assert.Equal(t, floatPtr(20), aggregateNumeric(api2.AggregationConfig_AVERAGE, samples))
	assert.Equal(t, floatPtr(10), aggregateNumeric(api2.AggregationConfig_MIN, samples))
	assert.Equal(t, floatPtr(30), aggregateNumeric(api2.AggregationConfig_MAX, samples))
	assert.Equal(t, floatPtr(60), aggregateNumeric(api2.AggregationConfig_SUM, samples))
	assert.Nil(t, aggregateNumeric(api2.AggregationConfig_AVERAGE, nil))

	latestSamples := []numericSample{{value: 1, lastReported: 100}, {value: 2, lastReported: 300}, {value: 3, lastReported: 200}}
	assert.Equal(t, floatPtr(2), aggregateNumeric(api2.AggregationConfig_LATEST, latestSamples))
}

func TestAggregateBool(t *testing.T) {
	trueVal, falseVal := true, false

	assert.Equal(t, &trueVal, aggregateBool(api2.AggregationConfig_ANY, []numericSample{{value: 0}, {value: 1}}))
	assert.Equal(t, &falseVal, aggregateBool(api2.AggregationConfig_ANY, []numericSample{{value: 0}, {value: 0}}))
	assert.Nil(t, aggregateBool(api2.AggregationConfig_ANY, nil))

	// LATEST picks the most recently reported sample regardless of value.
	assert.Equal(t, &falseVal, aggregateBool(api2.AggregationConfig_LATEST, []numericSample{{value: 1, lastReported: 100}, {value: 0, lastReported: 200}}))

	// A strategy with no meaning for a boolean metric falls back to ANY.
	assert.Equal(t, &trueVal, aggregateBool(api2.AggregationConfig_SUM, []numericSample{{value: 0}, {value: 1}}))
}

func TestComputeProperties_DefaultsPerMetric(t *testing.T) {
	sensors := []*apiDevice.Sensor{
		{
			Presence:      &apiTrait.Presence{State: &apiTrait.Presence_State{MotionDetected: true}},
			AirProperties: &apiTrait.AirProperties{State: &apiTrait.AirProperties_State{TemperatureC: 20}}, // 68F
			LightLevel:    &apiTrait.LightLevel{State: &apiTrait.LightLevel_State{Lux: 100}},
			Power:         &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 40}},
		},
		{
			Presence:      &apiTrait.Presence{State: &apiTrait.Presence_State{MotionDetected: false}},
			AirProperties: &apiTrait.AirProperties{State: &apiTrait.AirProperties_State{TemperatureC: 24}}, // 75.2F
			LightLevel:    &apiTrait.LightLevel{State: &apiTrait.LightLevel_State{Lux: 200}},
			Power:         &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 10}},
		},
	}

	props := computeProperties(nil, sensors)
	require.NotNil(t, props)

	// occupancy defaults to ANY: one of the two sensors saw motion.
	require.NotNil(t, props.Occupied)
	assert.True(t, *props.Occupied)

	// temperature defaults to AVERAGE: (68 + 75.2) / 2.
	require.NotNil(t, props.TemperatureF)
	assert.InDelta(t, 71.6, *props.TemperatureF, 0.01)

	// light defaults to AVERAGE.
	require.NotNil(t, props.LightLevelLux)
	assert.Equal(t, int32(150), *props.LightLevelLux)

	// power defaults to SUM.
	require.NotNil(t, props.PowerDrawW)
	assert.Equal(t, 50.0, *props.PowerDrawW)

	// No sensor reported air quality - left unset, not zeroed.
	assert.Nil(t, props.AirQualityIndex)
}

func TestComputeProperties_OverrideStrategy(t *testing.T) {
	sensors := []*apiDevice.Sensor{
		{Power: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 40}}},
		{Power: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 10}}},
	}

	cfg := &api2.AggregationConfig{PowerStrategy: api2.AggregationConfig_MAX}
	props := computeProperties(cfg, sensors)
	require.NotNil(t, props.PowerDrawW)
	assert.Equal(t, 40.0, *props.PowerDrawW)
}

func TestComputeProperties_NoSensors(t *testing.T) {
	assert.Nil(t, computeProperties(nil, nil))
}

func TestAggregationConfig_DBAPIRoundTrip(t *testing.T) {
	assert.Nil(t, dbAggregationToAPI(nil))
	assert.Nil(t, apiAggregationToDB(nil))

	dbCfg := &db.AggregationConfig{
		OccupancyStrategy:   db.AggregationAny,
		TemperatureStrategy: db.AggregationAverage,
		LightStrategy:       db.AggregationMax,
		AirQualityStrategy:  db.AggregationMin,
		PowerStrategy:       db.AggregationSum,
	}
	apiCfg := dbAggregationToAPI(dbCfg)
	require.NotNil(t, apiCfg)
	assert.Equal(t, api2.AggregationConfig_ANY, apiCfg.OccupancyStrategy)
	assert.Equal(t, api2.AggregationConfig_SUM, apiCfg.PowerStrategy)

	back := apiAggregationToDB(apiCfg)
	require.NotNil(t, back)
	assert.Equal(t, *dbCfg, *back)
}

func TestAggregator_LinkUnlinkAndUpdate(t *testing.T) {
	a := newAggregator(zaptest.NewLogger(t))

	a.setDeviceRoom("sensor-1", "room-1")
	assert.Nil(t, a.getProperties("room-1"), "no reading received yet")

	a.handleUpdate(&api2.Update{
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
			Device: &apiDevice.Device{
				Id:     "sensor-1",
				Config: &apiDevice.Device_Config{Name: "Kitchen Sensor"},
				Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
					Metadata: &apiDevice.Sensor_Metadata{LastReported: timestamppb.Now()},
					Presence: &apiTrait.Presence{State: &apiTrait.Presence_State{MotionDetected: true}},
				}},
			},
		}},
	})

	props := a.getProperties("room-1")
	require.NotNil(t, props)
	require.NotNil(t, props.Occupied)
	assert.True(t, *props.Occupied)

	// A device update for a device that isn't linked to any room is ignored.
	a.handleUpdate(&api2.Update{
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
			Device: &apiDevice.Device{
				Id: "sensor-unlinked",
				Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
					Presence: &apiTrait.Presence{State: &apiTrait.Presence_State{MotionDetected: true}},
				}},
			},
		}},
	})
	assert.Nil(t, a.getProperties("room-unlinked"))

	// Unlinking drops the sensor's contribution - room-1 goes back to no
	// linked sensor having ever reported.
	a.removeDeviceRoom("sensor-1")
	assert.Nil(t, a.getProperties("room-1"))
}

func TestAggregator_RoomAggregationOverride(t *testing.T) {
	a := newAggregator(zaptest.NewLogger(t))
	a.setDeviceRoom("sensor-1", "room-1")
	a.setDeviceRoom("sensor-2", "room-1")

	send := func(id string, w float64) {
		a.handleUpdate(&api2.Update{
			Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
				Device: &apiDevice.Device{
					Id: id,
					Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
						Power: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: w}},
					}},
				},
			}},
		})
	}
	send("sensor-1", 40)
	send("sensor-2", 10)

	// Default is SUM.
	require.NotNil(t, a.getProperties("room-1").PowerDrawW)
	assert.Equal(t, 50.0, *a.getProperties("room-1").PowerDrawW)

	a.setRoomAggregation("room-1", &api2.AggregationConfig{PowerStrategy: api2.AggregationConfig_MAX})
	require.NotNil(t, a.getProperties("room-1").PowerDrawW)
	assert.Equal(t, 40.0, *a.getProperties("room-1").PowerDrawW)
}
