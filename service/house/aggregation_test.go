package house

import (
	"testing"
	"time"

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
func int32Ptr(i int32) *int32     { return &i }

// sensorDevice wraps s as the Sensor-kind Device computeProperties/
// handleUpdate expect - most tests below only care about the Sensor
// payload, not the rest of Device's fields.
func sensorDevice(s *apiDevice.Sensor) *apiDevice.Device {
	return &apiDevice.Device{Details: &apiDevice.Device_Sensor{Sensor: s}}
}

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
	devices := []*apiDevice.Device{
		sensorDevice(&apiDevice.Sensor{
			Presence:      &apiTrait.Presence{State: &apiTrait.Presence_State{MotionDetected: true}},
			AirProperties: &apiTrait.AirProperties{State: &apiTrait.AirProperties_State{TemperatureC: 20}},
			LightLevel:    &apiTrait.LightLevel{State: &apiTrait.LightLevel_State{Lux: 100}},
			Power:         &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 40}},
		}),
		sensorDevice(&apiDevice.Sensor{
			Presence:      &apiTrait.Presence{State: &apiTrait.Presence_State{MotionDetected: false}},
			AirProperties: &apiTrait.AirProperties{State: &apiTrait.AirProperties_State{TemperatureC: 24}},
			LightLevel:    &apiTrait.LightLevel{State: &apiTrait.LightLevel_State{Lux: 200}},
			Power:         &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 10}},
		}),
	}

	props := computeProperties(nil, devices)
	require.NotNil(t, props)

	// occupancy defaults to ANY: one of the two sensors saw motion.
	require.NotNil(t, props.Occupied)
	assert.True(t, *props.Occupied)

	// temperature defaults to AVERAGE, in Celsius: (20 + 24) / 2.
	require.NotNil(t, props.TemperatureC)
	assert.InDelta(t, 22.0, *props.TemperatureC, 0.01)

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
	devices := []*apiDevice.Device{
		sensorDevice(&apiDevice.Sensor{Power: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 40}}}),
		sensorDevice(&apiDevice.Sensor{Power: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 10}}}),
	}

	cfg := &api2.AggregationConfig{PowerStrategy: api2.AggregationConfig_MAX}
	props := computeProperties(cfg, devices)
	require.NotNil(t, props.PowerDrawW)
	assert.Equal(t, 40.0, *props.PowerDrawW)
}

func TestComputeProperties_NoSensors(t *testing.T) {
	assert.Nil(t, computeProperties(nil, nil))
}

// TestDeviceLastReported_FallsBackToLastSeen covers the precedence bug fix:
// no bridge in this repo actually sets Sensor.Metadata.LastReported today,
// so a Sensor must still fall back to Device.LastSeen rather than silently
// returning 0 just because it's the Sensor kind.
func TestDeviceLastReported_FallsBackToLastSeen(t *testing.T) {
	seen := timestamppb.Now()

	sensorNoMetadata := &apiDevice.Device{
		LastSeen: seen,
		Details:  &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{}},
	}
	assert.Equal(t, seen.AsTime().UnixNano(), deviceLastReported(sensorNoMetadata))

	reported := timestamppb.New(seen.AsTime().Add(-time.Hour))
	sensorWithMetadata := &apiDevice.Device{
		LastSeen: seen,
		Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
			Metadata: &apiDevice.Sensor_Metadata{LastReported: reported},
		}},
	}
	assert.Equal(t, reported.AsTime().UnixNano(), deviceLastReported(sensorWithMetadata))

	nonSensor := &apiDevice.Device{
		LastSeen: seen,
		Details:  &apiDevice.Device_Ups{Ups: &apiDevice.UPS{}},
	}
	assert.Equal(t, seen.AsTime().UnixNano(), deviceLastReported(nonSensor))

	assert.Equal(t, int64(0), deviceLastReported(&apiDevice.Device{}))
}

// TestComputeProperties_NilSubState guards against a panic when a Sensor
// advertises a Presence/AirQuality trait whose State submessage hasn't been
// populated yet (e.g. a device that reports its traits before its first
// reading) - State is a proto3 message field, so it can be non-nil-trait,
// nil-state.
func TestComputeProperties_NilSubState(t *testing.T) {
	devices := []*apiDevice.Device{
		sensorDevice(&apiDevice.Sensor{
			Presence:   &apiTrait.Presence{},
			AirQuality: &apiTrait.AirQuality{},
		}),
	}

	var props *api2.Room_Properties
	require.NotPanics(t, func() {
		props = computeProperties(nil, devices)
	})
	// Neither trait's State was populated, so neither contributed a sample -
	// the room has no computed Properties at all, not a misleading
	// all-unset one (same contract as TestComputeProperties_NoSensors).
	assert.Nil(t, props)
}

// TestComputeProperties_NilSubState_AirPropertiesLightLevelPower is the
// same nil-sub-state guard as TestComputeProperties_NilSubState, for the
// three traits that didn't get one originally (AirProperties/LightLevel/
// Power) - exercised via Thermostat/Generic specifically, since those
// kinds only started routing through extractDeviceTraits's generic scan in
// this change, widening how often an unpopulated State is actually
// reachable.
func TestComputeProperties_NilSubState_AirPropertiesLightLevelPower(t *testing.T) {
	devices := []*apiDevice.Device{
		{Details: &apiDevice.Device_Thermostat{Thermostat: &apiDevice.Thermostat{
			AirProperties: &apiTrait.AirProperties{},
			Power:         &apiTrait.Power{},
		}}},
		{Details: &apiDevice.Device_Generic{Generic: &apiDevice.Generic{
			LightLevel: &apiTrait.LightLevel{},
		}}},
	}

	var props *api2.Room_Properties
	require.NotPanics(t, func() {
		props = computeProperties(nil, devices)
	})
	// No trait's State was populated anywhere - must not be read as a
	// genuine 0°C/0 lux/0W reading.
	assert.Nil(t, props)
}

// TestComputeProperties_AirQualitySubmetrics covers CO2/VOC/radon, which
// (unlike aqi) a device may report without ever reporting aqi itself - e.g.
// an Airthings sensor. All three share air_quality_strategy with aqi (see
// AggregationConfig.air_quality_strategy's doc comment), rather than each
// getting its own strategy field.
func TestComputeProperties_AirQualitySubmetrics(t *testing.T) {
	devices := []*apiDevice.Device{
		sensorDevice(&apiDevice.Sensor{AirQuality: &apiTrait.AirQuality{State: &apiTrait.AirQuality_State{
			Co2Ppm:                      int32Ptr(600),
			VolatileOrganicCompoundsPpb: int32Ptr(120),
			RadonBqM3:                   int32Ptr(40),
		}}}),
		sensorDevice(&apiDevice.Sensor{AirQuality: &apiTrait.AirQuality{State: &apiTrait.AirQuality_State{
			Co2Ppm: int32Ptr(800),
			// No VOC/radon from this one - shouldn't drag the average down.
		}}}),
	}

	props := computeProperties(nil, devices)
	require.NotNil(t, props)

	require.NotNil(t, props.Co2Ppm)
	assert.Equal(t, int32(700), *props.Co2Ppm) // AVERAGE of 600, 800

	require.NotNil(t, props.VocPpb)
	assert.Equal(t, int32(120), *props.VocPpb) // only one sample

	require.NotNil(t, props.RadonBqM3)
	assert.Equal(t, int32(40), *props.RadonBqM3)

	// No device reported aqi itself - left unset, not zeroed.
	assert.Nil(t, props.AirQualityIndex)
}

// TestComputeProperties_NonSensorKinds covers the device kinds other than
// Sensor that also carry Power/AirProperties/Presence - Thermostat, UPS,
// EVCharger, Generic (see api/device/*.proto) - all previously ignored
// entirely by computeProperties.
func TestComputeProperties_NonSensorKinds(t *testing.T) {
	devices := []*apiDevice.Device{
		{Details: &apiDevice.Device_Thermostat{Thermostat: &apiDevice.Thermostat{
			AirProperties: &apiTrait.AirProperties{State: &apiTrait.AirProperties_State{TemperatureC: 21}},
			Power:         &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 5}},
			Presence:      &apiTrait.Presence{State: &apiTrait.Presence_State{MotionDetected: true}},
		}}},
		{Details: &apiDevice.Device_Ups{Ups: &apiDevice.UPS{
			Power: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 15}},
		}}},
		{Details: &apiDevice.Device_EvCharger{EvCharger: &apiDevice.EVCharger{
			WallPower:    &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 1000}},
			VehiclePower: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 900}},
			// exterior_conditions deliberately has no bearing on room
			// temperature - verified below by it NOT pulling TemperatureC
			// away from the thermostat's 21.
			ExteriorConditions: &apiTrait.AirProperties{State: &apiTrait.AirProperties_State{TemperatureC: -10}},
		}}},
		{Details: &apiDevice.Device_Generic{Generic: &apiDevice.Generic{
			LightLevel: &apiTrait.LightLevel{State: &apiTrait.LightLevel_State{Lux: 50}},
			AirQuality: &apiTrait.AirQuality{State: &apiTrait.AirQuality_State{Aqi: int32Ptr(42)}},
		}}},
	}

	props := computeProperties(nil, devices)
	require.NotNil(t, props)

	// Only the thermostat's indoor reading counts - the EV charger's
	// exterior_conditions (-10) must not be averaged in.
	require.NotNil(t, props.TemperatureC)
	assert.Equal(t, 21.0, *props.TemperatureC)

	// SUM across thermostat (5) + UPS (15) + EV charger wall+vehicle (1000+900).
	require.NotNil(t, props.PowerDrawW)
	assert.Equal(t, 1920.0, *props.PowerDrawW)

	require.NotNil(t, props.Occupied)
	assert.True(t, *props.Occupied)

	require.NotNil(t, props.LightLevelLux)
	assert.Equal(t, int32(50), *props.LightLevelLux)

	require.NotNil(t, props.AirQualityIndex)
	assert.Equal(t, int32(42), *props.AirQualityIndex)
}

// TestComputeProperties_FanTemperature covers Fan's Temperature trait
// (api/trait/temperature.proto, distinct from AirProperties) - included in
// room temperature only when its own unit is actually Celsius, since unlike
// AirProperties.State.TemperatureC it carries no fixed unit of its own.
func TestComputeProperties_FanTemperature(t *testing.T) {
	celsius := &apiDevice.Device{Details: &apiDevice.Device_Fan{Fan: &apiDevice.Fan{
		Temperature: &apiTrait.Temperature{
			Attributes: &apiTrait.Temperature_Attributes{Unit: "celsius"},
			State:      &apiTrait.Temperature_State{Value: 19},
		},
	}}}
	props := computeProperties(nil, []*apiDevice.Device{celsius})
	require.NotNil(t, props)
	require.NotNil(t, props.TemperatureC)
	assert.Equal(t, 19.0, *props.TemperatureC)

	fahrenheit := &apiDevice.Device{Details: &apiDevice.Device_Fan{Fan: &apiDevice.Fan{
		Temperature: &apiTrait.Temperature{
			Attributes: &apiTrait.Temperature_Attributes{Unit: "fahrenheit"},
			State:      &apiTrait.Temperature_State{Value: 66},
		},
	}}}
	// A non-Celsius reading must not be silently treated as Celsius - with
	// nothing else reporting, the room has no computed Properties at all.
	assert.Nil(t, computeProperties(nil, []*apiDevice.Device{fahrenheit}))
}

// TestComputeProperties_CameraPresence demonstrates extractDeviceTraits's
// generic trait scan: Camera (api/device/camera.proto) was never on any
// hardcoded device-kind list this file used to keep, but it does declare a
// trait.Presence field, so a camera with motion detection genuinely
// contributes to room occupancy - correct generalized behavior, not a bug.
func TestComputeProperties_CameraPresence(t *testing.T) {
	camera := &apiDevice.Device{Details: &apiDevice.Device_Camera{Camera: &apiDevice.Camera{
		Presence: &apiTrait.Presence{State: &apiTrait.Presence_State{MotionDetected: true}},
	}}}
	props := computeProperties(nil, []*apiDevice.Device{camera})
	require.NotNil(t, props)
	require.NotNil(t, props.Occupied)
	assert.True(t, *props.Occupied)
}

// TestComputeProperties_FanTemperatureNilState guards against the same
// class of nil-sub-state bug TestComputeProperties_NilSubState covers for
// Presence/AirQuality - a Fan whose Temperature trait has Attributes set
// (so the unit check passes) but no State yet must not be read as a
// genuine 0.0 reading.
func TestComputeProperties_FanTemperatureNilState(t *testing.T) {
	fan := &apiDevice.Device{Details: &apiDevice.Device_Fan{Fan: &apiDevice.Fan{
		Temperature: &apiTrait.Temperature{
			Attributes: &apiTrait.Temperature_Attributes{Unit: "celsius"},
		},
	}}}
	assert.Nil(t, computeProperties(nil, []*apiDevice.Device{fan}))
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
	// The room's first real reading publishes both a RoomUpdate (new
	// Properties) and a BuildingUpdate (occupied goes from unknown to true).
	var ru *api2.RoomUpdate
	var bu *api2.BuildingUpdate
	for i := 0; i < 2; i++ {
		select {
		case msg := <-sink.Messages():
			hu, ok := msg.(*api2.HouseUpdate)
			require.True(t, ok)
			if r := hu.GetRoom(); r != nil {
				ru = r
			} else {
				bu = hu.GetBuilding()
			}
		default:
			t.Fatal("expected both a RoomUpdate and a BuildingUpdate on the first real reading")
		}
	}
	require.NotNil(t, ru)
	assert.Equal(t, "room-1", ru.RoomId)
	require.NotNil(t, ru.Properties.Occupied)
	assert.True(t, ru.Properties.GetOccupied())
	require.NotNil(t, bu)
	assert.Equal(t, "building-1", bu.BuildingId)
	require.NotNil(t, bu.State.Occupied)
	assert.True(t, bu.State.GetOccupied())

	// The identical reading again produces no new computed Properties and no
	// occupied change, so nothing is published at all (see
	// recomputeRoomLocked's proto.Equal check and
	// refreshBuildingOccupiedLocked's *bool dedup).
	send(true)
	select {
	case msg := <-sink.Messages():
		t.Fatalf("unexpected duplicate update: %+v", msg)
	default:
	}

	// A genuinely different reading does publish the room's own update again
	// - but lastMotion is never cleared on a false reading (see its doc
	// comment), so the building stays occupied and no second BuildingUpdate
	// follows.
	send(false)
	select {
	case msg := <-sink.Messages():
		hu := msg.(*api2.HouseUpdate)
		assert.False(t, hu.GetRoom().GetProperties().GetOccupied())
	default:
		t.Fatal("expected a RoomUpdate when occupancy actually changed")
	}
	select {
	case msg := <-sink.Messages():
		t.Fatalf("unexpected extra update: %+v", msg)
	default:
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

// motionUpdate builds a BridgeService Update reporting deviceID as a Sensor
// with Presence.motion_detected = motion - the same shape
// TestComputeProperties_DefaultsPerMetric uses to feed room occupancy.
func motionUpdate(deviceID string, motion bool) *api2.Update {
	return &api2.Update{
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
			Device: &apiDevice.Device{
				Id: deviceID,
				Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
					Presence: &apiTrait.Presence{State: &apiTrait.Presence_State{MotionDetected: motion}},
				}},
			},
		}},
	}
}

func TestAggregator_BuildingOccupied(t *testing.T) {
	a := newAggregator(zaptest.NewLogger(t))
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }

	a.registerRoom("room-1", "building-1")
	a.registerRoom("room-2", "building-1")
	a.setDeviceRoom("sensor-1", "room-1")
	a.setDeviceRoom("sensor-2", "room-2")

	// No room has ever reported anything - unknown, not false.
	assert.Nil(t, a.buildingOccupied("building-1"))
	assert.Nil(t, a.buildingOccupied("other-building"))

	a.handleUpdate(motionUpdate("sensor-1", true))
	require.NotNil(t, a.buildingOccupied("building-1"))
	assert.True(t, *a.buildingOccupied("building-1"))

	// Motion clears on the device, but the building stays occupied until
	// buildingOccupiedWindow has elapsed since the last true reading.
	a.handleUpdate(motionUpdate("sensor-1", false))
	now = now.Add(buildingOccupiedWindow - time.Second)
	require.NotNil(t, a.buildingOccupied("building-1"))
	assert.True(t, *a.buildingOccupied("building-1"))

	// Once the window has fully elapsed with no further motion, it's known
	// false, not nil - a signal has been seen, just not recently.
	now = now.Add(2 * time.Second)
	require.NotNil(t, a.buildingOccupied("building-1"))
	assert.False(t, *a.buildingOccupied("building-1"))

	// A second room reporting motion re-occupies the whole building.
	a.handleUpdate(motionUpdate("sensor-2", true))
	require.NotNil(t, a.buildingOccupied("building-1"))
	assert.True(t, *a.buildingOccupied("building-1"))

	// Removing every room that ever reported motion goes back to unknown.
	a.removeRoom("room-1")
	a.removeRoom("room-2")
	assert.Nil(t, a.buildingOccupied("building-1"))
}

// TestAggregator_DecayTimerPublishesOccupiedFalse covers the push side of
// occupied's decay: nothing short of a timer would ever notice a building
// going unoccupied with no further room activity to trigger a recheck (see
// armDecayTimerLocked). afterFunc is overridden to hand the scheduled
// callback back to the test instead of actually waiting
// buildingOccupiedWindow - the test fires it itself once it has advanced
// now, rather than waiting for a real 10 minutes.
func TestAggregator_DecayTimerPublishesOccupiedFalse(t *testing.T) {
	a := newAggregator(zaptest.NewLogger(t))
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }

	fired := make(chan func(), 1)
	a.afterFunc = func(d time.Duration, f func()) *time.Timer {
		assert.Equal(t, buildingOccupiedWindow, d)
		fired <- f
		return nil
	}

	a.registerRoom("room-1", "building-1")
	a.setDeviceRoom("sensor-1", "room-1")

	sink := a.updates.NewSink()
	defer sink.Close()

	a.handleUpdate(motionUpdate("sensor-1", true))

	// The first real reading publishes a RoomUpdate and a BuildingUpdate
	// (occupied: unknown -> true), and arms the decay timer.
	var bu *api2.BuildingUpdate
	for i := 0; i < 2; i++ {
		select {
		case msg := <-sink.Messages():
			if b := msg.(*api2.HouseUpdate).GetBuilding(); b != nil {
				bu = b
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for the initial updates")
		}
	}
	require.NotNil(t, bu)
	assert.True(t, bu.State.GetOccupied())

	var decayFn func()
	select {
	case decayFn = <-fired:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the decay timer to be armed")
	}

	// Advance past the window, then fire the timer as if it had actually
	// waited that long.
	now = now.Add(buildingOccupiedWindow + time.Second)
	decayFn()

	select {
	case msg := <-sink.Messages():
		bu := msg.(*api2.HouseUpdate).GetBuilding()
		require.NotNil(t, bu)
		assert.Equal(t, "building-1", bu.BuildingId)
		require.NotNil(t, bu.State.Occupied)
		assert.False(t, bu.State.GetOccupied())
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the decay's BuildingUpdate")
	}
}

// TestAggregator_UnrelatedUpdateDoesNotRearmDecayTimer guards against a real
// bug caught in review: armDecayTimerLocked must only be (re)armed by an
// actual fresh motion reading (inside recomputeRoomLocked), never by
// refreshBuildingOccupiedLocked running for some unrelated reason - a
// non-motion update to another device in the same building must not push
// the real decay deadline out past when the motion itself would warrant.
func TestAggregator_UnrelatedUpdateDoesNotRearmDecayTimer(t *testing.T) {
	a := newAggregator(zaptest.NewLogger(t))
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }

	fired := make(chan func(), 2)
	a.afterFunc = func(d time.Duration, f func()) *time.Timer {
		fired <- f
		return nil
	}

	a.registerRoom("room-1", "building-1")
	a.registerRoom("room-2", "building-1")
	a.setDeviceRoom("sensor-1", "room-1")
	a.setDeviceRoom("sensor-2", "room-2")

	sink := a.updates.NewSink()
	defer sink.Close()

	a.handleUpdate(motionUpdate("sensor-1", true))
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the decay timer to be armed")
	}
	// Drain the RoomUpdate and BuildingUpdate this first reading published.
	for i := 0; i < 2; i++ {
		select {
		case <-sink.Messages():
		case <-time.After(time.Second):
			t.Fatal("timed out draining the initial updates")
		}
	}

	// An unrelated, non-motion update to a different device in the same
	// building must not arm a second timer.
	a.handleUpdate(&api2.Update{
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
			Device: &apiDevice.Device{
				Id: "sensor-2",
				Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
					Power: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 5}},
				}},
			},
		}},
	})
	select {
	case <-fired:
		t.Fatal("unrelated update must not rearm the decay timer")
	default:
	}
}
