package metrics

import (
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

// Service turns BridgeService updates into points on a Sink. HandleUpdate is
// shaped for bridgeconn.Conn.Run's onUpdate callback: it does only in-memory
// work (the Sink buffers), so it never back-pressures the stream.
type Service struct {
	logger  *zap.Logger
	sink    Sink
	sampler *Sampler
	rooms   RoomLookup
	now     func() time.Time
}

// NewService creates a Service. rooms may be nil, in which case room_id is
// left off every point.
func NewService(logger *zap.Logger, sink Sink, sampler *Sampler, rooms RoomLookup) *Service {
	return &Service{logger: logger, sink: sink, sampler: sampler, rooms: rooms, now: time.Now}
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
			return
		}
		if d := du.GetDevice(); d != nil {
			s.handleDevice(du.GetBridgeId(), d, now)
		}
	}
}

func (s *Service) handleDevice(bridgeID string, d *apiDevice.Device, now time.Time) {
	if d.GetId() == "" {
		return
	}

	var points []Point
	for _, ts := range Flatten(d) {
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
				"bridge_id":   bridgeID,
				"device_id":   d.GetId(),
				"device_name": d.GetConfig().GetName(),
				"room_id":     s.roomID(d.GetId()),
				"trait":       ts.Trait,
				"source":      SourceLive,
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
