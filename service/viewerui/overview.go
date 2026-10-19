package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
	weatherv1 "github.com/rmrobinson/house/api/weather/v1"
	"github.com/rmrobinson/house/service/lib/houseview"
)

// The overview is the landing page: whole-house tiles (mode, occupancy,
// total power, temperatures, leaks), every thermostat's setpoint, and one
// motion row per floor. It's live: handleOverviewEvents re-renders the body
// on change, and pops up the cameras of an exterior room when motion starts
// there (see motion_popup in templates/partials/overview.html).

// houseSnapshot is everything the overview is computed from: one building
// plus its floors and each floor's rooms (devices included).
type houseSnapshot struct {
	Building *api2.Building
	Floors   []*api2.Floor
	Rooms    map[string][]*api2.Room // by floor ID
	// Devices is every device the bridge facade knows, linked to a room or
	// not (a dead sensor nobody linked still matters). Empty falls back to
	// just the rooms' devices - see allDevices.
	Devices []*apiDevice.Device
	// Weather is the newest weather-server reading, nil when there's no
	// usable one; WeatherNote then says why.
	Weather     *weatherv1.WeatherReading
	WeatherNote string
}

func (s *Server) loadSnapshot(ctx context.Context, buildingID string) (houseSnapshot, error) {
	snap := houseSnapshot{Rooms: map[string][]*api2.Room{}}
	var err error
	if snap.Building, err = s.house.GetBuilding(ctx, &api2.GetBuildingRequest{Id: buildingID}); err != nil {
		return snap, err
	}
	if snap.Floors, err = houseview.ListFloors(ctx, s.house, buildingID); err != nil {
		return snap, err
	}
	sortFloors(snap.Floors)
	for _, f := range snap.Floors {
		rooms, err := houseview.ListRoomsByFloor(ctx, s.house, f.GetId())
		if err != nil {
			return snap, err
		}
		sortRooms(rooms)
		snap.Rooms[f.GetId()] = rooms
	}
	if resp, err := s.bridge.ListDevices(ctx, &api2.ListDevicesRequest{}); err != nil {
		s.logger.Warn("overview: unable to list all devices, using room-linked ones only", zap.Error(err))
	} else {
		snap.Devices = resp.GetDevices()
	}
	snap.Weather, snap.WeatherNote = s.weather.current(time.Now())
	return snap, nil
}

// exteriorRooms is the set of room IDs on an exterior floor, mapped to
// whether each is currently occupied (motion).
func (snap houseSnapshot) exteriorRooms() map[string]bool {
	out := map[string]bool{}
	for _, f := range snap.Floors {
		if !f.GetExterior() {
			continue
		}
		for _, r := range snap.Rooms[f.GetId()] {
			out[r.GetId()] = r.GetProperties().GetOccupied()
		}
	}
	return out
}

type tileView struct {
	Label string
	Value string
	// Alert paints the value red (a leak).
	Alert bool
	// Note is dim text under the value, e.g. why a tile has no value.
	Note string
}

type thermostatView struct {
	Name     string
	Setpoint string
	Current  string
	Mode     string
	Hold     bool
}

type floorMotionView struct {
	ID       string
	Name     string
	Exterior bool
	// Motion is "yes", "no" or "unknown" - the same vocabulary as a room's
	// occupancy dot, so it reuses the dot CSS.
	Motion string
	// Active names the rooms currently showing motion.
	Active []string
}

func (f floorMotionView) MotionLabel() string {
	switch f.Motion {
	case "yes":
		return "motion"
	case "no":
		return "no motion"
	default:
		return "motion unknown"
	}
}

type overviewView struct {
	BuildingID string
	Name       string
	// Tiles is the HOUSE box (mode, occupancy, power, leaks). StatusTiles is
	// the STATUS box: things that need attention (UPS, unreachable/low-battery
	// devices, lights left on). The CLIMATE box has two sub-headings:
	// Interior (indoor temp, humidity, worst air quality, then each
	// thermostat) and Exterior (the weather server's reading).
	Tiles       []tileView
	StatusTiles []tileView
	Interior    []tileView
	Exterior    []tileView
	Thermostats []thermostatView
	Media       []mediaView
	Chargers    []chargerView
	Floors      []floorMotionView
	// ExteriorActive is true while any exterior room shows motion; the
	// client keys the camera popup's auto-close off it.
	ExteriorActive bool
}

// buildOverview computes the overview from snap. Pure, so it's unit-tested
// without a server.
func buildOverview(snap houseSnapshot) overviewView {
	v := overviewView{BuildingID: snap.Building.GetId(), Name: snap.Building.GetConfig().GetName()}

	var (
		powerW, indoorSum  float64
		havePower, indoorN int
		leaks              []string
		co2, voc, radon    = worstAir{label: "CO₂", unit: "ppm", alertAt: co2AlertPpm}, worstAir{label: "VOC", unit: "ppb", alertAt: vocAlertPpb}, worstAir{label: "Radon", unit: "Bq/m³", alertAt: radonAlertBqM3}
	)
	for _, f := range snap.Floors {
		fm := floorMotionView{ID: f.GetId(), Name: f.GetName(), Exterior: f.GetExterior(), Motion: "unknown"}
		for _, r := range snap.Rooms[f.GetId()] {
			p := r.GetProperties()
			if p == nil {
				p = &api2.Room_Properties{} // no linked device has reported yet
			}
			if p.Occupied != nil {
				if fm.Motion == "unknown" {
					fm.Motion = "no"
				}
				if p.GetOccupied() {
					fm.Motion = "yes"
					fm.Active = append(fm.Active, r.GetConfig().GetName())
				}
			}
			if p.PowerDrawW != nil {
				powerW += p.GetPowerDrawW()
				havePower++
			}
			if p.TemperatureC != nil {
				// An exterior room's temperature is not the indoor average.
				if !f.GetExterior() {
					indoorSum += p.GetTemperatureC()
					indoorN++
				}
			}
			if !f.GetExterior() {
				name := r.GetConfig().GetName()
				if p.Co2Ppm != nil {
					co2.add(float64(p.GetCo2Ppm()), name)
				}
				if p.VocPpb != nil {
					voc.add(float64(p.GetVocPpb()), name)
				}
				if p.RadonBqM3 != nil {
					radon.add(float64(p.GetRadonBqM3()), name)
				}
			}
			if p.GetWaterDetected() {
				leaks = append(leaks, r.GetConfig().GetName())
			}
		}
		if fm.Motion == "yes" && fm.Exterior {
			v.ExteriorActive = true
		}
		v.Floors = append(v.Floors, fm)
	}

	for _, d := range houseview.SortDevices(snap.allDevices()) {
		if d.GetThermostat() != nil {
			v.Thermostats = append(v.Thermostats, thermostatToView(d))
		}
	}

	state := snap.Building.GetState()
	v.Tiles = append(v.Tiles, tileView{Label: "Mode", Value: orDash(strings.ToUpper(state.GetMode()))})
	occ := ""
	if state != nil && state.Occupied != nil {
		occ = map[bool]string{true: "Occupied", false: "Vacant"}[state.GetOccupied()]
	}
	v.Tiles = append(v.Tiles, tileView{Label: "House", Value: orDash(occ)})
	power := ""
	if havePower > 0 {
		power = formatWatts(powerW)
	}
	v.Tiles = append(v.Tiles, tileView{Label: "Power", Value: orDash(power), Note: "metered devices only"})
	indoor := ""
	if indoorN > 0 {
		indoor = fmt.Sprintf("%.1f°C", indoorSum/float64(indoorN))
	}
	v.Interior = append(v.Interior, tileView{Label: "Temp", Value: orDash(indoor), Note: "room average"})
	v.Interior = append(v.Interior, humidityTile(snap), co2.tile(), voc.tile(), radon.tile())
	v.Exterior = exteriorTiles(snap.Weather, snap.WeatherNote)
	leak := tileView{Label: "Water", Value: "Dry"}
	if len(leaks) > 0 {
		leak = tileView{Label: "Water", Value: "Detected", Alert: true, Note: strings.Join(leaks, ", ")}
	}
	v.Tiles = append(v.Tiles, leak)

	v.StatusTiles = statusTiles(snap)
	v.Media = mediaPlaying(snap)
	v.Chargers = chargers(snap)
	return v
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func formatWatts(w float64) string {
	if w >= 1000 {
		return fmt.Sprintf("%.2f kW", w/1000)
	}
	return fmt.Sprintf("%.0f W", w)
}

// thermostatToView picks the setpoint that's actually in effect for the
// thermostat's target mode: heat's for HEAT, cool's for COOL, the band for
// AUTO, none for OFF.
func thermostatToView(d *apiDevice.Device) thermostatView {
	st := d.GetThermostat().GetThermostat().GetState()
	if st == nil {
		st = &apiTrait.Thermostat_State{}
	}
	tv := thermostatView{
		Name:     houseview.DisplayName(d),
		Current:  "—",
		Setpoint: "—",
		Hold:     st.GetHoldActive(),
	}
	if st.GetCurrentTemperatureCelsius() != 0 {
		tv.Current = fmt.Sprintf("%.1f°C", st.GetCurrentTemperatureCelsius())
	}
	switch st.GetTargetMode() {
	case apiTrait.Thermostat_HEAT:
		tv.Mode = "Heat"
		if st.HeatSetpointCelsius != nil {
			tv.Setpoint = fmt.Sprintf("%.1f°C", st.GetHeatSetpointCelsius())
		}
	case apiTrait.Thermostat_COOL:
		tv.Mode = "Cool"
		if st.CoolSetpointCelsius != nil {
			tv.Setpoint = fmt.Sprintf("%.1f°C", st.GetCoolSetpointCelsius())
		}
	case apiTrait.Thermostat_AUTO:
		tv.Mode = "Auto"
		if st.HeatSetpointCelsius != nil && st.CoolSetpointCelsius != nil {
			tv.Setpoint = fmt.Sprintf("%.1f–%.1f°C", st.GetHeatSetpointCelsius(), st.GetCoolSetpointCelsius())
		}
	case apiTrait.Thermostat_OFF:
		tv.Mode, tv.Setpoint = "Off", "Off"
	}
	return tv
}

type overviewPageData struct {
	ID       string
	Name     string
	Overview overviewView
	// Trends is whether to show the TRENDS box (a metrics server is configured).
	Trends bool
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snap, err := s.loadSnapshot(r.Context(), id)
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	ov := buildOverview(snap)
	s.renderPage(w, "overview", overviewPageData{ID: id, Name: ov.Name, Overview: ov, Trends: s.metrics != nil})
}

// popupCamera is one camera in the motion popup.
type popupCamera struct {
	Name    string
	URL     string
	Message string
}

type motionPopupView struct {
	RoomName   string
	Cameras    []popupCamera
	ICEServers string
}

// handlePopup renders the camera overlay for a room - every camera linked to
// it. 204 (so htmx swaps nothing) when the room has none.
func (s *Server) handlePopup(w http.ResponseWriter, r *http.Request) {
	room, err := s.house.GetRoom(r.Context(), &api2.GetRoomRequest{Id: r.PathValue("id")})
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	pv := motionPopupView{RoomName: room.GetConfig().GetName(), ICEServers: s.iceServersJSON}
	for _, d := range houseview.SortDevices(room.GetDevices()) {
		if d.GetCamera() == nil {
			continue
		}
		pc := popupCamera{Name: houseview.DisplayName(d)}
		pc.URL, pc.Message = whepURL(d.GetCamera().GetMediaStream())
		pv.Cameras = append(pv.Cameras, pc)
	}
	if len(pv.Cameras) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.renderFragment(w, "motion_popup", pv)
}

// A re-render costs a ListFloors + a ListRooms per floor (each resolving its
// devices) + ListDevices, and device telemetry can arrive every few seconds,
// so renders are coalesced: the first change after a quiet spell renders
// after overviewDebounce, and no tab renders more often than overviewMinGap.
// Both are vars so tests can shorten them.
var (
	overviewDebounce = time.Second
	overviewMinGap   = 5 * time.Second
)

// handleOverviewEvents keeps the overview live over SSE. Every house or
// thermostat update schedules a (debounced) full re-render of the body; on
// top of that, motion starting in an exterior room opens the camera popup,
// and the exterior-activity flag is pushed so the page can auto-close it.
func (s *Server) handleOverviewEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.httpError(w, r, fmt.Errorf("streaming unsupported"))
		return
	}
	ctx := r.Context()
	buildingID := r.PathValue("id")

	snap, err := s.loadSnapshot(ctx, buildingID)
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	// exterior maps exterior room ID -> last seen occupied.
	exterior := snap.exteriorRooms()
	active := anyTrue(exterior)

	roomUpdates, unsubRooms := s.roomHub(buildingID).Subscribe()
	defer unsubRooms()
	devUpdates, unsubDevs := s.devHub.Subscribe()
	defer unsubDevs()

	// A nil channel (no weather server configured) never fires.
	var weatherUpdates <-chan *weatherv1.WeatherReading
	if s.weather != nil {
		var unsubWeather func()
		weatherUpdates, unsubWeather = s.weather.hub.Subscribe()
		defer unsubWeather()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	var (
		refresh    <-chan time.Time // nil until a re-render is pending
		lastRender time.Time
	)
	schedule := func() {
		if refresh == nil {
			refresh = time.After(max(overviewDebounce, overviewMinGap-time.Since(lastRender)))
		}
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()

		case hu := <-roomUpdates:
			schedule()
			ru := hu.GetRoom()
			if ru == nil {
				continue
			}
			prev, isExterior := exterior[ru.GetRoomId()]
			if !isExterior {
				continue
			}
			now := ru.GetProperties().GetOccupied()
			exterior[ru.GetRoomId()] = now
			if now && !prev {
				if !s.writeFragments(w, "popup_open_oob", struct{ RoomID string }{ru.GetRoomId()}) {
					return
				}
			}
			if a := anyTrue(exterior); a != active {
				active = a
				if !s.writeFragments(w, "exterior_state_oob", struct{ Active bool }{active}) {
					return
				}
			}
			flusher.Flush()

		case u := <-devUpdates:
			// Thermostats, media, lights, UPSes, batteries and reachability
			// all feed the overview; the debounce absorbs chatty telemetry.
			if u.GetDeviceUpdate().GetDevice() != nil {
				schedule()
			}

		case <-weatherUpdates:
			schedule()

		case <-refresh:
			refresh = nil
			lastRender = time.Now()
			cur, err := s.loadSnapshot(ctx, buildingID)
			if err != nil {
				s.logger.Warn("overview: unable to refresh", zap.String("building_id", buildingID), zap.Error(err))
				continue
			}
			if !s.writeFragments(w, "overview_oob", buildOverview(cur)) {
				return
			}
			flusher.Flush()
		}
	}
}

func anyTrue(m map[string]bool) bool {
	for _, v := range m {
		if v {
			return true
		}
	}
	return false
}
