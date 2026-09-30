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

// TestComputeProperties_NilSubState guards against a panic when a Sensor
// advertises a Presence/AirQuality trait whose State submessage hasn't been
// populated yet (e.g. a device that reports its traits before its first
// reading) - State is a proto3 message field, so it can be non-nil-trait,
// nil-state.
func TestComputeProperties_NilSubState(t *testing.T) {
	sensors := []*apiDevice.Sensor{
		{
			Presence:   &apiTrait.Presence{},
			AirQuality: &apiTrait.AirQuality{},
		},
	}

	var props *api2.Room_Properties
	require.NotPanics(t, func() {
		props = computeProperties(nil, sensors)
	})
	// Neither trait's State was populated, so neither contributed a sample -
	// the room has no computed Properties at all, not a misleading
	// all-unset one (same contract as TestComputeProperties_NoSensors).
	assert.Nil(t, props)
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

func TestAggregator_PublishesOnlyWhenPropertiesChange(t *testing.T) {
	a := newAggregator(zaptest.NewLogger(t))
	a.registerRoom("room-1", "building-1")
	a.setDeviceRoom("sensor-1", "room-1")

	sink := a.updates.NewSink()
	defer sink.Close()

	send := func(motion bool) {
		a.handleUpdate(&api2.Update{
			Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
				Device: &apiDevice.Device{
					Id: "sensor-1",
					Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
						Presence: &apiTrait.Presence{State: &apiTrait.Presence_State{MotionDetected: motion}},
					}},
				},
			}},
		})
	}

	send(true)
	select {
	case msg := <-sink.Messages():
		ru, ok := msg.(*api2.RoomUpdate)
		require.True(t, ok)
		assert.Equal(t, "room-1", ru.RoomId)
		require.NotNil(t, ru.Properties.Occupied)
		assert.True(t, ru.Properties.GetOccupied())
	default:
		t.Fatal("expected a RoomUpdate on the first real reading")
	}

	// The identical reading again produces no new computed Properties, so
	// no update is published (see recomputeRoomLocked's proto.Equal check).
	send(true)
	select {
	case msg := <-sink.Messages():
		t.Fatalf("unexpected duplicate update: %+v", msg)
	default:
	}

	// A genuinely different reading does publish again.
	send(false)
	select {
	case msg := <-sink.Messages():
		ru := msg.(*api2.RoomUpdate)
		assert.False(t, ru.Properties.GetOccupied())
	default:
		t.Fatal("expected a RoomUpdate when occupancy actually changed")
	}
}

func TestAggregator_PropertiesForBuildingAndBuildingOf(t *testing.T) {
	a := newAggregator(zaptest.NewLogger(t))
	a.registerRoom("room-1", "building-1")
	a.registerRoom("room-2", "building-2")
	a.setDeviceRoom("sensor-1", "room-1")
	a.setDeviceRoom("sensor-2", "room-2")

	assert.Equal(t, "building-1", a.buildingOf("room-1"))
	assert.Equal(t, "building-2", a.buildingOf("room-2"))
	assert.Equal(t, "", a.buildingOf("unknown-room"))

	// Neither room has reported anything yet - nothing to snapshot.
	assert.Empty(t, a.propertiesForBuilding("building-1"))

	a.handleUpdate(&api2.Update{
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
			Device: &apiDevice.Device{
				Id: "sensor-1",
				Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
					Power: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 40}},
				}},
			},
		}},
	})
	a.handleUpdate(&api2.Update{
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
			Device: &apiDevice.Device{
				Id: "sensor-2",
				Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
					Power: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 10}},
				}},
			},
		}},
	})

	building1 := a.propertiesForBuilding("building-1")
	require.Len(t, building1, 1)
	require.Contains(t, building1, "room-1")
	assert.Equal(t, 40.0, building1["room-1"].GetPowerDrawW())

	building2 := a.propertiesForBuilding("building-2")
	require.Len(t, building2, 1)
	require.Contains(t, building2, "room-2")

	// registerRoom/removeRoom keep roomBuilding (and therefore
	// propertiesForBuilding/buildingOf) in sync with room lifecycle too.
	a.removeRoom("room-1")
	assert.Equal(t, "", a.buildingOf("room-1"))
	assert.Empty(t, a.propertiesForBuilding("building-1"))
}
