// Package houseview holds the read-side helpers every house web UI (adminui,
// viewerui) needs: device ordering/labelling, collecting HouseService's
// streaming List* RPCs into slices, and mapping gRPC errors to HTTP.
package houseview

import (
	"cmp"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	apiDevice "github.com/rmrobinson/house/api/device"
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
