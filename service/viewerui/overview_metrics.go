package main

import (
	"fmt"
	"strings"

	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/lib/houseview"
)

// Alert thresholds for the worst-room air quality tiles. CO2 and radon follow
// the usual guidance (1000 ppm is where indoor air starts to feel stale;
// Health Canada's radon action level is 200 Bq/m³); VOC is a rule of thumb.
const (
	co2AlertPpm     = 1000
	vocAlertPpb     = 500
	radonAlertBqM3  = 200
	lowBatteryPct   = 10 // matches the daily battery-report policy's "action needed"
	evChargingWatts = 500
	maxNamed        = 3 // names listed in a tile's note before "+N more"
)

// allDevices is snap.Devices plus any room-linked device not already in it,
// each once.
func (snap houseSnapshot) allDevices() []*apiDevice.Device {
	seen := map[string]bool{}
	var out []*apiDevice.Device
	add := func(d *apiDevice.Device) {
		if !seen[d.GetId()] {
			seen[d.GetId()] = true
			out = append(out, d)
		}
	}
	for _, d := range snap.Devices {
		add(d)
	}
	for _, rooms := range snap.Rooms {
		for _, r := range rooms {
			for _, d := range r.GetDevices() {
				add(d)
			}
		}
	}
	return out
}

type roomRef struct {
	Name     string
	Exterior bool
}

// deviceRooms maps device ID -> the room it's linked to.
func (snap houseSnapshot) deviceRooms() map[string]roomRef {
	out := map[string]roomRef{}
	for _, f := range snap.Floors {
		for _, r := range snap.Rooms[f.GetId()] {
			for _, d := range r.GetDevices() {
				out[d.GetId()] = roomRef{Name: r.GetConfig().GetName(), Exterior: f.GetExterior()}
			}
		}
	}
	return out
}

// worstAir tracks the highest reading of one air-quality metric across
// interior rooms.
type worstAir struct {
	label, unit string
	alertAt     float64
	max         float64
	room        string
	seen        bool
}

func (w *worstAir) add(v float64, room string) {
	if !w.seen || v > w.max {
		w.max, w.room, w.seen = v, room, true
	}
}

func (w worstAir) tile() tileView {
	t := tileView{Label: w.label + " (worst)", Value: "—"}
	if w.seen {
		t.Value = fmt.Sprintf("%.0f %s", w.max, w.unit)
		t.Note = w.room
		t.Alert = w.max >= w.alertAt
	}
	return t
}

// humidityTile averages relative humidity over every interior sensor and
// thermostat reporting it (0 means "not measured" in AirProperties).
func humidityTile(snap houseSnapshot) tileView {
	rooms := snap.deviceRooms()
	var sum float64
	var n int
	for _, d := range snap.allDevices() {
		if rooms[d.GetId()].Exterior {
			continue
		}
		air := d.GetSensor().GetAirProperties()
		if air == nil {
			air = d.GetThermostat().GetAirProperties()
		}
		if h := air.GetState().GetHumidityPercentage(); h > 0 {
			sum += float64(h)
			n++
		}
	}
	t := tileView{Label: "Humidity", Value: "—", Note: "average"}
	if n > 0 {
		t.Value = fmt.Sprintf("%.0f%%", sum/float64(n))
	}
	return t
}

// statusTiles are the "needs attention" tiles: UPS, device health, lights.
func statusTiles(snap houseSnapshot) []tileView {
	devices := snap.allDevices()
	return []tileView{upsTile(devices), deviceHealthTile(devices), lightsTile(devices)}
}

// upsTile: alert while any UPS is discharging, else the shortest runtime.
func upsTile(devices []*apiDevice.Device) tileView {
	t := tileView{Label: "UPS", Value: "—"}
	var onBattery []string
	shortest, shortestName := int32(-1), ""
	for _, d := range devices {
		if d.GetUps() == nil {
			continue
		}
		st := d.GetUps().GetBattery().GetState()
		if st.GetDischarging() {
			onBattery = append(onBattery, houseview.DisplayName(d))
		}
		if mins := st.GetCapacityRemainingMins(); mins > 0 && (shortest < 0 || mins < shortest) {
			shortest, shortestName = mins, houseview.DisplayName(d)
		}
	}
	switch {
	case len(onBattery) > 0:
		t.Value, t.Alert, t.Note = "On battery", true, namesNote(onBattery)
	case shortest >= 0:
		t.Value, t.Note = houseview.FormatRuntime(shortest), shortestName+" · shortest runtime"
	}
	return t
}

// deviceHealthTile counts unreachable devices (the same kinds the daily
// battery-report covers) and low-battery ones.
func deviceHealthTile(devices []*apiDevice.Device) tileView {
	var unreachable, low []string
	for _, d := range devices {
		name := houseview.DisplayName(d)
		switch {
		case d.GetLight() != nil, d.GetSensor() != nil, d.GetGeneric() != nil, d.GetFan() != nil:
			if !d.GetAddress().GetIsReachable() {
				unreachable = append(unreachable, name)
				continue // an unreachable device's battery reading is stale
			}
		}
		batt := d.GetSensor().GetBattery()
		if batt == nil {
			batt = d.GetGeneric().GetBattery()
		}
		// A battery trait with no state is "not reported yet", not 0%.
		if (batt.GetState() != nil && batt.GetState().GetCapacityRemainingPct() < lowBatteryPct) || d.GetSensor().GetMetadata().GetLowBattery() {
			low = append(low, name)
		}
	}
	t := tileView{Label: "Devices", Value: "All OK"}
	var parts, names []string
	if len(unreachable) > 0 {
		parts = append(parts, fmt.Sprintf("%d offline", len(unreachable)))
		names = append(names, unreachable...)
	}
	if len(low) > 0 {
		parts = append(parts, fmt.Sprintf("%d low battery", len(low)))
		names = append(names, low...)
	}
	if len(parts) > 0 {
		t.Value, t.Alert, t.Note = strings.Join(parts, " · "), true, namesNote(names)
	}
	return t
}

func lightsTile(devices []*apiDevice.Device) tileView {
	on, total := 0, 0
	for _, d := range devices {
		if d.GetLight() == nil {
			continue
		}
		total++
		// An unreachable light's last state is stale; don't claim it's on.
		if d.GetAddress().GetIsReachable() && d.GetLight().GetOnOff().GetState().GetIsOn() {
			on++
		}
	}
	t := tileView{Label: "Lights on", Value: "—"}
	if total > 0 {
		t.Value, t.Note = fmt.Sprintf("%d of %d", on, total), "lights"
	}
	return t
}

// namesNote lists the first few names, then "+N more".
func namesNote(names []string) string {
	if len(names) <= maxNamed {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s +%d more", strings.Join(names[:maxNamed], ", "), len(names)-maxNamed)
}

type mediaView struct {
	Name    string
	Room    string
	Summary string
}

// mediaPlaying lists every media device with something playing or paused.
func mediaPlaying(snap houseSnapshot) []mediaView {
	rooms := snap.deviceRooms()
	var out []mediaView
	for _, d := range houseview.SortDevices(snap.allDevices()) {
		sum := houseview.MediaSummary(houseview.Media(d), houseview.App(d).GetState().GetApplicationName())
		if sum != "" {
			out = append(out, mediaView{Name: houseview.DisplayName(d), Room: rooms[d.GetId()].Name, Summary: sum})
		}
	}
	return out
}

type chargerView struct {
	Name  string
	State string // "Charging", "Idle" or "Offline"
	Power string
	// Session is "12.3 kWh · 1h 5m" while the charger reports a session.
	Session string
}

func chargers(snap houseSnapshot) []chargerView {
	var out []chargerView
	for _, d := range houseview.SortDevices(snap.allDevices()) {
		ev := d.GetEvCharger()
		if ev == nil {
			continue
		}
		cv := chargerView{Name: houseview.DisplayName(d), State: "Idle"}
		watts := ev.GetWallPower().GetState().GetPowerW()
		cv.Power = formatWatts(watts)
		if !d.GetAddress().GetIsReachable() {
			cv.State, cv.Power = "Offline", "—"
		} else if watts >= evChargingWatts {
			cv.State = "Charging"
		}
		if sess := ev.GetChargingSession().GetState(); sess.GetEnergyWh() > 0 {
			cv.Session = fmt.Sprintf("%.1f kWh · %s", sess.GetEnergyWh()/1000, houseview.FormatRuntime(sess.GetDurationS()/60))
		}
		out = append(out, cv)
	}
	return out
}
