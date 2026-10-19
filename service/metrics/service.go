package metrics

import (
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/lib/protoreflectutil"
)

const (
	// StateMeasurement holds device trait state, one point per trait.
	StateMeasurement = "state"
	// CommandMeasurement holds one point per command a bridge processed.
	CommandMeasurement = "command"

	// SourceLive tags points written from the live update stream.
	SourceLive = "live"
	// SourceBackfill tags points written from a bridge's on-device history.
	SourceBackfill = "backfill"
)

// Excludes lists state that is never recorded. It applies to device state
// only: commands are always recorded.
type Excludes struct {
	// Details are Device.details oneof field names, e.g. "connected_device"
	// (the omada bridge's network clients).
	Details []string
	// BridgeIDs excludes every device a bridge reports.
	BridgeIDs []string
	// DeviceIDs excludes individual devices.
	DeviceIDs []string
}

// DefaultExcludes leaves out network clients (what the omada bridge reports),
// which are numerous and, for now, noise.
func DefaultExcludes() Excludes {
	return Excludes{Details: []string{"connected_device"}}
}

// Service turns BridgeService updates into points on a Sink. HandleUpdate is
// shaped for bridgeconn.Conn.Run's onUpdate callback: it does only in-memory
// work (the Sink buffers), so it never back-pressures the stream.
type Service struct {
	logger  *zap.Logger
	sink    Sink
	sampler *Sampler
	rooms   RoomLookup
	now     func() time.Time

	excludeDetails map[string]bool
	excludeBridges map[string]bool
	excludeDevices map[string]bool

	historyMu      sync.Mutex
	historyDevices map[string]HistoryDevice
}

// HistoryDevice is a device advertising Sensor.Metadata.history, i.e. one a
// bridge can be asked for recorded samples of.
type HistoryDevice struct {
	DeviceID string
	BridgeID string
	// Name is Config.Name as last seen; it tags backfilled points.
	Name string
	// MaxWindow is the furthest back one request can reach.
	MaxWindow time.Duration
	// SampleInterval is the spacing of recorded samples.
	SampleInterval time.Duration
}

// HistoryDevices returns the devices seen so far that advertise history,
// ordered by device ID.
func (s *Service) HistoryDevices() []HistoryDevice {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	out := make([]HistoryDevice, 0, len(s.historyDevices))
	for _, h := range s.historyDevices {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out
}

func (s *Service) noteHistory(bridgeID string, d *apiDevice.Device) {
	h := d.GetSensor().GetMetadata().GetHistory()

	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	if h == nil {
		delete(s.historyDevices, d.GetId())
		return
	}
	if s.historyDevices == nil {
		s.historyDevices = make(map[string]HistoryDevice)
	}
	s.historyDevices[d.GetId()] = HistoryDevice{
		DeviceID:       d.GetId(),
		BridgeID:       bridgeID,
		Name:           d.GetConfig().GetName(),
		MaxWindow:      h.GetMaxWindow().AsDuration(),
		SampleInterval: h.GetSampleInterval().AsDuration(),
	}
}

// HistoryPoints converts a bridge-reported sample for h into points tagged
// source=backfill. They bypass the sampler (a historical fact is never
// "unchanged") and the exclusion lists (a device is only asked for history
// because it was recorded live). Re-writing a sample is harmless: Influx
// overwrites a point with identical measurement, tags and timestamp.
func (s *Service) HistoryPoints(h HistoryDevice, sample *api2.HistorySample) []Point {
	if sample.GetTime() == nil || sample.GetSnapshot() == nil {
		return nil
	}
	at := sample.GetTime().AsTime()

	var points []Point
	for _, ts := range Flatten(sample.GetSnapshot()) {
		fields := make(map[string]FieldValue, len(ts.Leaves))
		for _, l := range ts.Leaves {
			fields[l.Name] = l.Value
		}
		points = append(points, Point{
			Measurement: StateMeasurement,
			Tags: map[string]string{
				"bridge_id":    h.BridgeID,
				"device_id":    h.DeviceID,
				"device_name":  h.Name,
				"device_label": s.deviceLabel(h.DeviceID, h.Name),
				"room_id":      s.roomID(h.DeviceID),
				"trait":        ts.Trait,
				"source":       SourceBackfill,
			},
			Fields: fields,
			Time:   at,
		})
	}
	return points
}

// SetExcludes replaces the exclusion lists. Call it before HandleUpdate is
// in use; it is not safe to call concurrently with it.
func (s *Service) SetExcludes(e Excludes) {
	set := func(vs []string) map[string]bool {
		m := make(map[string]bool, len(vs))
		for _, v := range vs {
			m[v] = true
		}
		return m
	}
	s.excludeDetails = set(e.Details)
	s.excludeBridges = set(e.BridgeIDs)
	s.excludeDevices = set(e.DeviceIDs)
}

func (s *Service) excluded(bridgeID string, d *apiDevice.Device) bool {
	if s.excludeBridges[bridgeID] || s.excludeDevices[d.GetId()] {
		return true
	}
	if len(s.excludeDetails) > 0 {
		if _, name, ok := protoreflectutil.OneofMessage(d, "details"); ok && s.excludeDetails[string(name)] {
			return true
		}
	}
	return false
}

// NewService creates a Service. rooms may be nil, in which case room_id is
// left off every point.
func NewService(logger *zap.Logger, sink Sink, sampler *Sampler, rooms RoomLookup) *Service {
	s := &Service{logger: logger, sink: sink, sampler: sampler, rooms: rooms, now: time.Now}
	s.SetExcludes(DefaultExcludes())
	return s
}

// HandleUpdate processes one update from the bridge stream.
//
// An INITIAL replay (what a (re)connect delivers) goes through the same
// sampler as live changes, so after a reconnect only values that actually
// differ from what was last written - or have exceeded the heartbeat - are
// written again.
func (s *Service) HandleUpdate(u *api2.Update) {
	now := s.now()

	if ev := u.GetCommandEvent(); ev != nil {
		s.handleCommand(ev, now)
		return
	}

	if iu := u.GetInitialUpdate(); iu != nil {
		for _, d := range iu.GetDevices() {
			s.handleDevice(iu.GetBridge().GetId(), d, now)
		}
		return
	}

	if du := u.GetDeviceUpdate(); du != nil {
		if u.GetAction() == api2.Update_REMOVED {
			s.sampler.Forget(du.GetDeviceId())
			s.historyMu.Lock()
			delete(s.historyDevices, du.GetDeviceId())
			s.historyMu.Unlock()
			return
		}
		if d := du.GetDevice(); d != nil {
			s.handleDevice(du.GetBridgeId(), d, now)
		}
	}
}

func (s *Service) handleDevice(bridgeID string, d *apiDevice.Device, now time.Time) {
	if d.GetId() == "" || s.excluded(bridgeID, d) {
		return
	}
	s.noteHistory(bridgeID, d)

	traits := Flatten(d)
	// Reachability lives on Device.Address rather than in a trait, but a
	// device dropping offline is exactly what a history of it should show.
	if d.GetAddress() != nil {
		traits = append(traits, TraitState{Trait: "reachability", Leaves: []Leaf{
			{Name: "is_reachable", Value: FieldValue{Kind: KindBool, Bool: d.GetAddress().GetIsReachable()}},
		}})
	}

	var points []Point
	for _, ts := range traits {
		leaves := s.sampler.Filter(d.GetId(), ts.Trait, ts.Leaves, now)
		if len(leaves) == 0 {
			continue
		}

		fields := make(map[string]FieldValue, len(leaves))
		for _, l := range leaves {
			fields[l.Name] = l.Value
		}
		points = append(points, Point{
			Measurement: StateMeasurement,
			Tags: map[string]string{
				"bridge_id":    bridgeID,
				"device_id":    d.GetId(),
				"device_name":  d.GetConfig().GetName(),
				"device_label": s.deviceLabel(d.GetId(), d.GetConfig().GetName()),
				"room_id":      s.roomID(d.GetId()),
				"trait":        ts.Trait,
				"source":       SourceLive,
			},
			Fields: fields,
			Time:   now,
		})
	}

	if len(points) > 0 {
		s.sink.Write(points...)
	}
}

func (s *Service) handleCommand(ev *api2.CommandEvent, now time.Time) {
	cmd := ev.GetCommand()
	if cmd == nil {
		return
	}

	commandType := "unknown"
	if _, name, ok := protoreflectutil.OneofMessage(cmd, "details"); ok {
		commandType = string(name)
	}

	detail, err := protojson.Marshal(cmd)
	if err != nil {
		s.logger.Warn("unable to encode command detail", zap.Error(err), zap.String("command_id", cmd.GetId()))
		detail = nil
	}

	at := now
	if ts := ev.GetTime(); ts != nil && ts.IsValid() {
		at = ts.AsTime()
	}

	fields := map[string]FieldValue{
		"outcome": {Kind: KindInt, Int: int64(ev.GetResult().GetCode())},
		"detail":  {Kind: KindString, Str: string(detail)},
	}
	if id := cmd.GetId(); id != "" {
		fields["command_id"] = FieldValue{Kind: KindString, Str: id}
	}
	if v := ev.GetResultingVersion(); v != "" {
		fields["resulting_version"] = FieldValue{Kind: KindString, Str: v}
	}

	s.sink.Write(Point{
		Measurement: CommandMeasurement,
		Tags: map[string]string{
			"bridge_id":    ev.GetBridgeId(),
			"device_id":    cmd.GetDeviceId(),
			"command_type": commandType,
			"origin":       cmd.GetOrigin(),
		},
		Fields: fields,
		Time:   at,
	})
}

func (s *Service) roomID(deviceID string) string {
	if s.rooms == nil {
		return ""
	}
	return s.rooms.RoomID(deviceID)
}

// deviceLabel is the name a person should see for a device on a chart, built
// the way the policy engine's notifications build it: the device's name
// followed by its room in parentheses, e.g. "Airthings Wave Plus (Office)".
// Device names alone aren't unique - two Wave Plus sensors are both just
// "Airthings Wave Plus" - so the room is what tells them apart. A device with
// no room gets its bare name, and one with no name falls back to its ID, as
// the policy engine's GetDeviceName does.
func (s *Service) deviceLabel(deviceID, deviceName string) string {
	name := deviceName
	if name == "" {
		name = deviceID
	}
	if s.rooms == nil {
		return name
	}
	if room := s.rooms.RoomName(deviceID); room != "" {
		return name + " (" + room + ")"
	}
	return name
}
