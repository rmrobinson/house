package main

import (
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/bridges/lib/homekitctrl"
)

// Remote sensor characteristic IDs, captured from the "Zone B" sensor paired alongside the
// thermostat - see docs/ecobee-hap-dump.txt. These happen to number the same as the thermostat's
// own built-in MotionSensor/OccupancySensor characteristics (see mapping.go) purely by
// coincidence of this specific capture, not because remote sensors are guaranteed to share iids
// with the thermostat - re-verify against your own capture if pairing additional/different
// sensors.
const (
	remoteSensorModelID      = "EBERS41"
	remoteSensorManufacturer = "ecobee Inc."
)

const (
	remoteSensorCharBatteryChargingState uint64 = 193
	remoteSensorCharBatteryLevel         uint64 = 194
	remoteSensorCharBatteryLowStatus     uint64 = 195

	remoteSensorCharCurrentTemperature uint64 = 209

	remoteSensorCharOccupancyDetected uint64 = 65
	remoteSensorCharMotionDetected    uint64 = 66
)

// remoteSensorCharIDs returns every characteristic this bridge reads on each poll cycle for a
// remote sensor accessory at the given aid.
func remoteSensorCharIDs(aid uint64) []homekitctrl.CharID {
	ids := []uint64{
		remoteSensorCharBatteryChargingState,
		remoteSensorCharBatteryLevel,
		remoteSensorCharBatteryLowStatus,
		remoteSensorCharCurrentTemperature,
		remoteSensorCharOccupancyDetected,
		remoteSensorCharMotionDetected,
	}
	out := make([]homekitctrl.CharID, len(ids))
	for i, id := range ids {
		out[i] = homekitctrl.CharID{AccessoryID: aid, CharacteristicID: id}
	}
	return out
}

// buildRemoteSensorDevice translates a remote sensor accessory's current characteristic values
// into a house device.Device. id is the house device ID to stamp onto it; name comes from the
// bridge's config (see buildThermostatDevice's doc comment for why).
func buildRemoteSensorDevice(id, name string, values charValues) *device.Device {
	d := &device.Device{
		Id:           id,
		ModelId:      remoteSensorModelID,
		Manufacturer: remoteSensorManufacturer,
		Config:       &device.Device_Config{Name: name},
		Details: &device.Device_Sensor{
			Sensor: &device.Sensor{
				AirProperties: &trait.AirProperties{
					Attributes: &trait.AirProperties_Attributes{},
					State:      &trait.AirProperties_State{},
				},
				Battery: &trait.Battery{
					Attributes: &trait.Battery_Attributes{},
					State:      &trait.Battery_State{},
				},
				Presence: &trait.Presence{
					Attributes: &trait.Presence_Attributes{},
					State:      &trait.Presence_State{},
				},
			},
		},
	}

	sensor := d.GetSensor()

	if v, ok := values.float(remoteSensorCharCurrentTemperature); ok {
		sensor.AirProperties.State.TemperatureC = float32(v)
	}
	if v, ok := values.int(remoteSensorCharBatteryLevel); ok {
		sensor.Battery.State.CapacityRemainingPct = int32(v)
	}
	if v, ok := values.int(remoteSensorCharBatteryChargingState); ok {
		// 0=not charging, 1=charging, 2=not chargeable (this sensor's non-rechargeable coin
		// cell) - "discharging" is true whenever it isn't actively charging.
		sensor.Battery.State.Discharging = v != 1
	}
	if v, ok := values.bool(remoteSensorCharMotionDetected); ok {
		sensor.Presence.State.MotionDetected = v
	}
	if v, ok := values.bool(remoteSensorCharOccupancyDetected); ok {
		sensor.Presence.State.OccupancyDetected = ptr(v)
	}

	return d
}
