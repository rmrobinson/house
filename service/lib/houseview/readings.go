package houseview

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
)

// Reading is one pre-formatted measurement a device reports, e.g.
// {"Humidity", "45%"}.
type Reading struct {
	Label string
	Value string
	// Alert marks a reading that wants attention (water detected).
	Alert bool
	// Metric is the "<trait>.<leaf>" key this reading's value is recorded
	// under in metricsd (the device message's field name for the trait, then
	// the state field), set only for numeric readings that make sense as a
	// time series. Empty for text and bool readings.
	Metric string
}

// Readings collects the measurements d reports across whichever of its traits
// carry any - temperature, humidity, power draw, battery, air quality, light
// level and so on - in a stable order. Zero values of the non-optional numeric
// fields are treated as "not reported", matching how Room aggregation treats
// them. Controls (on/off, brightness, media) are not readings; see viewerui's
// deviceToView for those.
//
// A device type holding several traits of the same kind (an EVCharger's wall
// and vehicle Power) has the trait's field name prefixed onto its labels.
func Readings(d *apiDevice.Device) []Reading {
	details := d.ProtoReflect().Descriptor().Oneofs().ByName("details")
	if details == nil {
		return nil
	}
	fd := d.ProtoReflect().WhichOneof(details)
	if fd == nil || fd.Message() == nil {
		return nil
	}
	dm := d.ProtoReflect().Get(fd).Message()

	var out []Reading
	fields := dm.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if f.Message() == nil || f.IsList() || f.IsMap() || !dm.Has(f) {
			continue
		}
		prefix := ""
		switch f.Name() {
		case "air_properties", "air_quality", "power", "battery", "light_level", "temperature", "ventilation", "water", "position":
		default:
			prefix = humanize(string(f.Name())) + " "
		}
		out = append(out, traitReadings(prefix, string(f.Name()), dm.Get(f).Message().Interface())...)
	}
	return out
}

func traitReadings(prefix, fieldName string, m proto.Message) []Reading {
	var out []Reading
	add := func(label, format string, args ...any) {
		out = append(out, Reading{Label: prefix + label, Value: fmt.Sprintf(format, args...)})
	}
	// addM is add for a numeric reading recorded under the state field leaf.
	addM := func(leaf, label, format string, args ...any) {
		add(label, format, args...)
		out[len(out)-1].Metric = fieldName + "." + leaf
	}
	switch t := m.(type) {
	case *apiTrait.Temperature:
		unit := "°C"
		if strings.HasPrefix(strings.ToLower(t.GetAttributes().GetUnit()), "f") {
			unit = "°F"
		}
		addM("value", "Temp", "%.1f%s", t.GetState().GetValue(), unit)
	case *apiTrait.AirProperties:
		s := t.GetState()
		if s.GetTemperatureC() != 0 || s.GetHumidityPercentage() != 0 || s.GetPressureHpa() != 0 {
			addM("temperature_c", "Temp", "%.1f°C", s.GetTemperatureC())
		}
		if s.GetHumidityPercentage() != 0 {
			addM("humidity_percentage", "Humidity", "%.0f%%", s.GetHumidityPercentage())
		}
		if s.GetPressureHpa() != 0 {
			addM("pressure_hpa", "Pressure", "%.0f hPa", s.GetPressureHpa())
		}
	case *apiTrait.AirQuality:
		s := t.GetState()
		if s.AirQuality != nil {
			add("Air quality", "%s", s.GetAirQuality())
		}
		if s.Aqi != nil {
			addM("aqi", "AQI", "%d", s.GetAqi())
		}
		if s.Co2Ppm != nil {
			addM("co2_ppm", "CO2", "%d ppm", s.GetCo2Ppm())
		}
		if s.VolatileOrganicCompoundsPpb != nil {
			addM("volatile_organic_compounds_ppb", "VOC", "%d ppb", s.GetVolatileOrganicCompoundsPpb())
		}
		if s.Pm2_5 != nil {
			addM("pm2_5", "PM2.5", "%d µg/m³", s.GetPm2_5())
		}
		if s.Pm10 != nil {
			addM("pm10", "PM10", "%d µg/m³", s.GetPm10())
		}
		if s.RadonBqM3 != nil {
			addM("radon_bq_m3", "Radon", "%d Bq/m³", s.GetRadonBqM3())
		}
		if s.CarbonMonoxideDetected != nil {
			out = append(out, Reading{Label: prefix + "CO", Value: detected(s.GetCarbonMonoxideDetected()), Alert: s.GetCarbonMonoxideDetected()})
		}
	case *apiTrait.Position:
		if t.GetAttributes().GetUnit() != "" {
			addM("value", "Position", "%.0f %s", t.GetState().GetValue(), t.GetAttributes().GetUnit())
		}
	case *apiTrait.Power:
		s := t.GetState()
		if s.GetPowerW() != 0 {
			addM("power_w", "Power", "%.1f W", s.GetPowerW())
		}
		if s.GetVoltageV() != 0 {
			addM("voltage_v", "Voltage", "%.1f V", s.GetVoltageV())
		}
		if s.GetCurrentA() != 0 {
			addM("current_a", "Current", "%.2f A", s.GetCurrentA())
		}
		if s.FrequencyHz != nil && s.GetFrequencyHz() != 0 {
			addM("frequency_hz", "Frequency", "%.1f Hz", s.GetFrequencyHz())
		}
		if s.EnergyKwh != nil {
			addM("energy_kwh", "Energy", "%.2f kWh", s.GetEnergyKwh())
		}
	case *apiTrait.Battery:
		s := t.GetState()
		if s.GetCapacityRemainingPct() != 0 {
			addM("capacity_remaining_pct", "Battery", "%d%%", s.GetCapacityRemainingPct())
		}
		if s.GetCapacityRemainingMins() != 0 {
			addM("capacity_remaining_mins", "Runtime", "%s", FormatRuntime(s.GetCapacityRemainingMins()))
		}
		if s.GetStatus() != "" {
			add("Status", "%s", s.GetStatus())
		}
	case *apiTrait.LightLevel:
		s := t.GetState()
		if s.GetLux() != 0 {
			addM("lux", "Light", "%.0f lux", s.GetLux())
		}
		if s.UvIndex != nil {
			addM("uv_index", "UV", "%d", s.GetUvIndex())
		}
	case *apiTrait.Ventilation:
		s := t.GetState()
		if s.MinutesRemaining != nil {
			add("Remaining", "%s", FormatRuntime(s.GetMinutesRemaining()))
		}
		if s.GetFilterStatusMessage() != "" {
			add("Filter", "%s", s.GetFilterStatusMessage())
		}
	case *apiDevice.Sensor_BinarySensor:
		// Only water is a measurement worth surfacing; alarm, fire and
		// opened_closed have no sensible label here.
		if fieldName == "water" {
			v := "Dry"
			if t.GetIsActive() {
				v = "Detected"
			}
			out = append(out, Reading{Label: "Water", Value: v, Alert: t.GetIsActive()})
		}
	}
	return out
}

func detected(b bool) string {
	if b {
		return "Detected"
	}
	return "Clear"
}

// humanize turns a proto field name ("wall_power") into "Wall power".
func humanize(name string) string {
	s := strings.ReplaceAll(name, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
