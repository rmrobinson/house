package main

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zaptest"

	apiDevice "github.com/rmrobinson/house/api/device"
)

func TestDevicesFilterFromRequest(t *testing.T) {
	// A plain page load reads its own query string.
	r := httptest.NewRequest("GET", "/devices?unlinked=1&connected=1", nil)
	assert.Equal(t, devicesFilter{Unlinked: true, Connected: true}, devicesFilterFromRequest(r))

	// A link/move POST has no query string of its own - the filter the page
	// was showing comes from htmx's HX-Current-URL instead, so it survives.
	r = httptest.NewRequest("POST", "/devices/d1/link", nil)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("HX-Current-URL", "http://adminui.local/devices?unlinked=1")
	assert.Equal(t, devicesFilter{Unlinked: true}, devicesFilterFromRequest(r))
}

func TestDevicesFilterToggles(t *testing.T) {
	var f devicesFilter
	assert.Equal(t, "/devices", f.URL())
	assert.Equal(t, "/devices?unlinked=1", f.ToggleUnlinked())
	assert.Equal(t, "/devices?connected=1", f.ToggleConnected())

	f = devicesFilter{Unlinked: true, Connected: true}
	assert.Equal(t, "/devices?connected=1&unlinked=1", f.URL())
	assert.Equal(t, "/devices?connected=1", f.ToggleUnlinked())
	assert.Equal(t, "/devices?unlinked=1", f.ToggleConnected())
}

func TestSortDevices(t *testing.T) {
	named := func(id, name, manufacturer string) *apiDevice.Device {
		return &apiDevice.Device{Id: id, Manufacturer: manufacturer, Config: &apiDevice.Device_Config{Name: name}}
	}
	devices := []*apiDevice.Device{
		named("4", "lamp", "Zeta"),
		named("3", "Lamp", "Acme"),
		named("2", "", ""), // no name - sorts by its ID, "2"
		named("1", "Kettle", ""),
		named("0", "lamp", "Acme"),
	}

	var ids []string
	for _, d := range sortDevices(devices) {
		ids = append(ids, d.GetId())
	}
	assert.Equal(t, []string{"2", "1", "0", "3", "4"}, ids)
}

func TestDevicesPageAndPickersRender(t *testing.T) {
	s := &Server{logger: zaptest.NewLogger(t)}

	rec := httptest.NewRecorder()
	s.renderPage(rec, "devices", devicesPageData{
		Filter:  devicesFilter{Connected: true},
		Devices: []deviceView{{ID: "d1", Name: "Lamp", Manufacturer: "Acme", Model: "L-100", Online: true}},
	})
	body := rec.Body.String()
	assert.Contains(t, body, "Acme")
	assert.Contains(t, body, "L-100")
	assert.Contains(t, body, `href="/devices?connected=1&amp;unlinked=1" class="filter-chip"`)
	assert.Contains(t, body, `href="/devices" class="filter-chip filter-chip-on"`)

	rec = httptest.NewRecorder()
	s.renderFragment(rec, "room_picker", roomPickerData{DeviceID: "d1", DeviceName: "Lamp", Rooms: []roomOption{{ID: "r1", Label: "Home · Main · Kitchen"}}})
	assert.Contains(t, rec.Body.String(), `<dialog class="picker-dialog">`)
	assert.Contains(t, rec.Body.String(), "Link / move Lamp")

	rec = httptest.NewRecorder()
	s.renderFragment(rec, "device_picker", devicePickerData{TargetRoomID: "r1", Devices: []devicePickerEntry{{ID: "d1", Name: "Lamp", Manufacturer: "Acme"}}})
	assert.Contains(t, rec.Body.String(), `<dialog class="picker-dialog">`)
	assert.Contains(t, rec.Body.String(), "Acme")
}
