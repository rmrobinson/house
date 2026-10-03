// Package houseview holds the read-side helpers every house web UI (adminui,
// viewerui) needs: device ordering/labelling, collecting HouseService's
// streaming List* RPCs into slices, and mapping gRPC errors to HTTP.
package houseview

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
)

// SortDevices sorts devices in place by display name (case-insensitively),
// then manufacturer, then ID, and returns it - every device table and picker
// lists devices in this one order, rather than whatever order the
// BridgeService facade happened to iterate its cache in (a Go map, so
// different on every request).
func SortDevices(devices []*apiDevice.Device) []*apiDevice.Device {
	slices.SortFunc(devices, func(a, b *apiDevice.Device) int {
		return cmp.Or(
			cmp.Compare(strings.ToLower(DisplayName(a)), strings.ToLower(DisplayName(b))),
			cmp.Compare(a.GetManufacturer(), b.GetManufacturer()),
			cmp.Compare(a.GetId(), b.GetId()),
		)
	})
	return devices
}

// DisplayName falls back to d's ID when Config.Name is unset - a device with
// no configured name would otherwise render as a blank label.
func DisplayName(d *apiDevice.Device) string {
	if name := d.GetConfig().GetName(); name != "" {
		return name
	}
	return d.GetId()
}

// Kind returns a short label for whichever "details" oneof case is set, for
// display only - mirrors the type switch shape used for dispatch in
// service/bridge/api.go's deviceSupportsCommand.
func Kind(d *apiDevice.Device) string {
	switch {
	case d.GetAvReceiver() != nil:
		return "AV Receiver"
	case d.GetClock() != nil:
		return "Clock"
	case d.GetLight() != nil:
		return "Light"
	case d.GetSensor() != nil:
		return "Sensor"
	case d.GetThermostat() != nil:
		return "Thermostat"
	case d.GetUps() != nil:
		return "UPS"
	case d.GetEvCharger() != nil:
		return "EV Charger"
	case d.GetMediaPlayer() != nil:
		return "Media Player"
	case d.GetTelevision() != nil:
		return "Television"
	case d.GetConnectedDevice() != nil:
		return "Connected Device"
	case d.GetCamera() != nil:
		return "Camera"
	case d.GetFan() != nil:
		return "Fan"
	case d.GetStandingDesk() != nil:
		return "Standing Desk"
	default:
		return "Generic"
	}
}

// RecvStream is the shape every HouseService List* streaming client shares -
// satisfied by api.HouseService_ListBuildingsClient, _ListFloorsClient and
// _ListRoomsClient without needing generics over the generated types
// themselves.
type RecvStream[T any] interface {
	Recv() (T, error)
}

// RecvAll drains stream, calling cb for each item until EOF or an error.
func RecvAll[T any](stream RecvStream[T], cb func(T) error) error {
	for {
		item, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := cb(item); err != nil {
			return err
		}
	}
}

// StreamAll collects every item a List* RPC streams back, via open (which
// starts the stream and forwards each item to cb).
func StreamAll[T any](open func(cb func(T) error) error) ([]T, error) {
	var items []T
	err := open(func(item T) error {
		items = append(items, item)
		return nil
	})
	return items, err
}

// HTTPStatus maps a gRPC status code (as returned by HouseService/
// BridgeService) to the closest HTTP status for display.
func HTTPStatus(err error) int {
	switch status.Code(err) {
	case codes.NotFound:
		return http.StatusNotFound
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.FailedPrecondition:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// Message extracts the human-readable message from a gRPC status error,
// falling back to err.Error() for anything else.
func Message(err error) string {
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}

// Properties mirrors Room.Properties, pre-formatted for display -
// each field is "" when that metric is unset (no linked Sensor has
// reported it yet), which the room templates treats as "Unknown".
type Properties struct {
	Occupied        string
	TemperatureC    string
	LightLevelLux   string
	AirQualityIndex string
	Co2Ppm          string
	VocPpb          string
	RadonBqM3       string
	PowerDrawW      string
	// WaterDetected is "Detected" or "Dry".
	WaterDetected string
	// BatteryRuntime is the shortest UPS runtime in the room, e.g. "42 min" or "1h 5m".
	BatteryRuntime string
}

// PropertiesToView formats p's set fields for display, leaving an unset
// metric (including every field, if p itself is nil - no linked device has
// reported anything for this room yet) as "". Temperature is always Celsius,
// matching Room.Properties.
func PropertiesToView(p *api2.Room_Properties) Properties {
	var pv Properties
	if p == nil {
		return pv
	}
	if p.Occupied != nil {
		if p.GetOccupied() {
			pv.Occupied = "Yes"
		} else {
			pv.Occupied = "No"
		}
	}
	if p.TemperatureC != nil {
		pv.TemperatureC = fmt.Sprintf("%.1f°C", p.GetTemperatureC())
	}
	if p.LightLevelLux != nil {
		pv.LightLevelLux = fmt.Sprintf("%d lux", p.GetLightLevelLux())
	}
	if p.AirQualityIndex != nil {
		pv.AirQualityIndex = fmt.Sprintf("%d", p.GetAirQualityIndex())
	}
	if p.Co2Ppm != nil {
		pv.Co2Ppm = fmt.Sprintf("%d ppm", p.GetCo2Ppm())
	}
	if p.VocPpb != nil {
		pv.VocPpb = fmt.Sprintf("%d ppb", p.GetVocPpb())
	}
	if p.RadonBqM3 != nil {
		pv.RadonBqM3 = fmt.Sprintf("%d Bq/m³", p.GetRadonBqM3())
	}
	if p.PowerDrawW != nil {
		pv.PowerDrawW = fmt.Sprintf("%.1f W", p.GetPowerDrawW())
	}
	if p.BatteryRuntimeMins != nil {
		pv.BatteryRuntime = formatRuntime(p.GetBatteryRuntimeMins())
	}
	if p.WaterDetected != nil {
		if p.GetWaterDetected() {
			pv.WaterDetected = "Detected"
		} else {
			pv.WaterDetected = "Dry"
		}
	}
	return pv
}

// formatRuntime renders a runtime in minutes as "N min" or, from an hour up, "Hh Mm".
func formatRuntime(mins int32) string {
	if mins < 60 {
		return fmt.Sprintf("%d min", mins)
	}
	return fmt.Sprintf("%dh %dm", mins/60, mins%60)
}

// OnOff returns d's OnOff trait, or nil if its device type has none - the
// trait sits at a different spot in each device type, so callers that just
// want "can I toggle this" go through here instead of switching themselves.
func OnOff(d *apiDevice.Device) *apiTrait.OnOff {
	switch {
	case d.GetLight() != nil:
		return d.GetLight().GetOnOff()
	case d.GetFan() != nil:
		return d.GetFan().GetOnOff()
	case d.GetAvReceiver() != nil:
		return d.GetAvReceiver().GetOnOff()
	case d.GetClock() != nil:
		return d.GetClock().GetOnOff()
	case d.GetEvCharger() != nil:
		return d.GetEvCharger().GetOnOff()
	case d.GetUps() != nil:
		return d.GetUps().GetOnOff()
	case d.GetTelevision() != nil:
		return d.GetTelevision().GetOnOff()
	case d.GetThermostat() != nil:
		return d.GetThermostat().GetOnOff()
	case d.GetGeneric() != nil:
		return d.GetGeneric().GetOnOff()
	default:
		return nil
	}
}

// Brightness returns d's Brightness trait, or nil if its device type has none.
func Brightness(d *apiDevice.Device) *apiTrait.Brightness {
	switch {
	case d.GetLight() != nil:
		return d.GetLight().GetBrightness()
	case d.GetClock() != nil:
		return d.GetClock().GetBrightness()
	case d.GetGeneric() != nil:
		return d.GetGeneric().GetBrightness()
	default:
		return nil
	}
}

// ListBuildings/ListFloors/ListRoomsByFloor wrap HouseService's streaming
// List* RPCs into a plain slice - every caller wants the whole list at once
// (home-scale result sets, not worth consuming the stream incrementally).
func ListBuildings(ctx context.Context, house api2.HouseServiceClient) ([]*api2.Building, error) {
	return StreamAll(func(cb func(*api2.Building) error) error {
		stream, err := house.ListBuildings(ctx, &api2.ListBuildingsRequest{})
		if err != nil {
			return err
		}
		return RecvAll(stream, cb)
	})
}

func ListFloors(ctx context.Context, house api2.HouseServiceClient, buildingID string) ([]*api2.Floor, error) {
	return StreamAll(func(cb func(*api2.Floor) error) error {
		stream, err := house.ListFloors(ctx, &api2.ListFloorsRequest{BuildingId: buildingID})
		if err != nil {
			return err
		}
		return RecvAll(stream, cb)
	})
}

func ListRoomsByFloor(ctx context.Context, house api2.HouseServiceClient, floorID string) ([]*api2.Room, error) {
	return StreamAll(func(cb func(*api2.Room) error) error {
		stream, err := house.ListRooms(ctx, &api2.ListRoomsRequest{FloorId: &floorID})
		if err != nil {
			return err
		}
		return RecvAll(stream, cb)
	})
}
