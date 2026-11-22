package agent

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
)

// fakeHouseClient is a minimal api2.HouseServiceClient stand-in -
// roomNamesByDeviceID only ever calls ListRooms, so every other method
// panics (via the embedded nil interface) if a test reaches it, same
// convention as house.deviceconfig_test.go's fakeBridgeServer.
type fakeHouseClient struct {
	api2.HouseServiceClient
	rooms []*api2.Room
}

func (f *fakeHouseClient) ListRooms(ctx context.Context, req *api2.ListRoomsRequest, opts ...grpc.CallOption) (api2.HouseService_ListRoomsClient, error) {
	return &fakeRoomStream{items: f.rooms}, nil
}

type fakeRoomStream struct {
	grpc.ClientStream
	items []*api2.Room
	i     int
}

func (s *fakeRoomStream) Recv() (*api2.Room, error) {
	if s.i >= len(s.items) {
		return nil, io.EOF
	}
	item := s.items[s.i]
	s.i++
	return item, nil
}

func TestRoomNamesByDeviceID(t *testing.T) {
	house := &fakeHouseClient{
		rooms: []*api2.Room{
			{
				Id:     "room1",
				Config: &api2.Room_Config{Name: "Office"},
				Devices: []*apiDevice.Device{
					{Id: "d1"},
				},
			},
			{
				Id:     "room2",
				Config: &api2.Room_Config{Name: "Primary Bedroom"},
				Devices: []*apiDevice.Device{
					{Id: "d2"},
				},
			},
			// A room with no devices at all must not panic or contribute
			// any entries.
			{Id: "room3", Config: &api2.Room_Config{Name: "Empty Room"}},
		},
	}
	a := &Agent{houseCfg: HouseServiceConfig{Client: house, BuildingID: "b1"}, logger: zap.NewNop()}

	names, err := a.roomNamesByDeviceID(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Office", names["d1"])
	assert.Equal(t, "Primary Bedroom", names["d2"])
	assert.Len(t, names, 2)
}

// TestRoomNamesByDeviceID_NoHouseClient covers the degrade-gracefully case:
// googlehome-agentd pointed at a standalone bridgefacaded (no HouseService
// at all) configures no houseCfg, and SYNC must not fail just because
// RoomHint can't be looked up.
func TestRoomNamesByDeviceID_NoHouseClient(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}

	names, err := a.roomNamesByDeviceID(context.Background())
	require.NoError(t, err)
	assert.Nil(t, names)
}

// TestRoomNamesByDeviceID_ClientSetButNoBuildingID covers a misconfigured
// HouseServiceConfig (Client set, BuildingID empty) - main.go's own
// startup check already prevents constructing one today, but this guards
// the invariant at the type's own point of use too, so a future caller
// can't silently query ListRooms with an empty BuildingId and get back
// zero rows with no signal anything is wrong.
func TestRoomNamesByDeviceID_ClientSetButNoBuildingID(t *testing.T) {
	house := &fakeHouseClient{rooms: []*api2.Room{
		{Id: "room1", Config: &api2.Room_Config{Name: "Office"}, Devices: []*apiDevice.Device{{Id: "d1"}}},
	}}
	a := &Agent{houseCfg: HouseServiceConfig{Client: house, BuildingID: ""}, logger: zap.NewNop()}

	names, err := a.roomNamesByDeviceID(context.Background())
	require.NoError(t, err)
	assert.Nil(t, names)
}

func cameraWithDetection(id string, visible ...*apiTrait.Detection_Object) *apiDevice.Device {
	return &apiDevice.Device{
		Id: id,
		Details: &apiDevice.Device_Camera{Camera: &apiDevice.Camera{
			Detection: &apiTrait.Detection{
				Attributes: &apiTrait.Detection_Attributes{Labels: []string{"person"}},
				State:      &apiTrait.Detection_State{Visible: visible},
			},
		}},
	}
}

func newNotifyAgent() *Agent {
	return &Agent{
		logger:         zap.NewNop(),
		knownDetection: map[string]map[string]struct{}{},
		knownWaterLeak: map[string]bool{},
	}
}

func TestDetectionNotification_NewUnfamiliarObject(t *testing.T) {
	a := newNotifyAgent()
	d := cameraWithDetection("camera1", &apiTrait.Detection_Object{Label: "person", EventId: "evt1"})

	notification, eventID, ok := a.detectionNotification(d)
	require.True(t, ok)
	assert.Equal(t, "evt1", eventID)
	objDetection := notification["ObjectDetection"].(map[string]interface{})
	assert.Equal(t, 0, objDetection["priority"])
	objects := objDetection["objects"].(map[string]interface{})
	assert.Equal(t, 1, objects["unfamiliar"])
	assert.Nil(t, objects["named"])
}

func TestDetectionNotification_NamedObject(t *testing.T) {
	a := newNotifyAgent()
	d := cameraWithDetection("camera1", &apiTrait.Detection_Object{Label: "person", SubLabel: "Alice", EventId: "evt1"})

	notification, _, ok := a.detectionNotification(d)
	require.True(t, ok)
	objects := notification["ObjectDetection"].(map[string]interface{})["objects"].(map[string]interface{})
	assert.Equal(t, []string{"Alice"}, objects["named"])
	assert.Nil(t, objects["unfamiliar"])
}

func TestDetectionNotification_AlreadySeenObjectNotRenotified(t *testing.T) {
	a := newNotifyAgent()
	obj := &apiTrait.Detection_Object{Label: "person", EventId: "evt1"}
	d := cameraWithDetection("camera1", obj)

	_, _, ok := a.detectionNotification(d)
	require.True(t, ok)

	// Same object still in Detection.state.visible on the next update - already reported, so
	// this must not fire again.
	_, _, ok = a.detectionNotification(d)
	assert.False(t, ok)
}

// TestDetectionNotification_TimestampIgnoresObjectOrder guards against a real bug: the first
// new object having no first_seen (frigate's FirstSeen can be its zero value) must not cause a
// later object's real, earlier timestamp to be discarded in favour of time.Now().
func TestDetectionNotification_TimestampIgnoresObjectOrder(t *testing.T) {
	a := newNotifyAgent()
	earlier := time.Now().Add(-time.Hour)
	d := cameraWithDetection("camera1",
		&apiTrait.Detection_Object{Label: "person", EventId: "evt1"}, // no FirstSeen
		&apiTrait.Detection_Object{Label: "person", EventId: "evt2", FirstSeen: timestamppb.New(earlier)},
	)

	notification, _, ok := a.detectionNotification(d)
	require.True(t, ok)
	gotMs := notification["ObjectDetection"].(map[string]interface{})["detectionTimestamp"].(int64)
	assert.Equal(t, earlier.UnixMilli(), gotMs)
}

func TestDetectionNotification_NoDetectionCapability(t *testing.T) {
	a := newNotifyAgent()
	d := &apiDevice.Device{Id: "camera1", Details: &apiDevice.Device_Camera{Camera: &apiDevice.Camera{}}}

	_, _, ok := a.detectionNotification(d)
	assert.False(t, ok)
}

func sensorWithWaterLeak(id string, isActive bool) *apiDevice.Device {
	return &apiDevice.Device{
		Id: id,
		Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
			Water: &apiDevice.Sensor_BinarySensor{IsActive: isActive},
		}},
	}
}

func TestWaterLeakNotification_TransitionToLeakFires(t *testing.T) {
	a := newNotifyAgent()
	d := sensorWithWaterLeak("sensor1", true)

	notification, _, ok := a.waterLeakNotification(d)
	require.True(t, ok)
	sensorState := notification["SensorState"].(map[string]interface{})
	assert.Equal(t, "WaterLeak", sensorState["name"])
	assert.Equal(t, "leak", sensorState["currentSensorState"])

	// Still active on the next update - already notified, so no repeat.
	_, _, ok = a.waterLeakNotification(d)
	assert.False(t, ok)
}

func TestWaterLeakNotification_NoLeakNeverFires(t *testing.T) {
	a := newNotifyAgent()
	d := sensorWithWaterLeak("sensor1", false)

	_, _, ok := a.waterLeakNotification(d)
	assert.False(t, ok)
}

func TestWaterLeakNotification_NoWaterSensor(t *testing.T) {
	a := newNotifyAgent()
	d := &apiDevice.Device{Id: "sensor1", Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{}}}

	_, _, ok := a.waterLeakNotification(d)
	assert.False(t, ok)
}
