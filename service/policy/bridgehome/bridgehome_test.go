package bridgehome

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/policy"
)

// fakeBridgeServer is a real (network-served) BridgeService standing in for
// either a single bridge or a facade - the test suite below exercises both
// shapes against the same Adapter code to prove it doesn't distinguish them.
type fakeBridgeServer struct {
	api2.UnimplementedBridgeServiceServer

	updates []*api2.Update

	mu      sync.Mutex
	cmds    []*command.Command
	cmdResp *device.Device
	cmdErr  error
}

func (s *fakeBridgeServer) StreamUpdates(req *api2.StreamUpdatesRequest, stream api2.BridgeService_StreamUpdatesServer) error {
	for _, u := range s.updates {
		if err := stream.Send(u); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func (s *fakeBridgeServer) ExecuteCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	s.mu.Lock()
	s.cmds = append(s.cmds, cmd)
	resp, err := s.cmdResp, s.cmdErr
	s.mu.Unlock()
	return resp, err
}

func (s *fakeBridgeServer) recordedCommands() []*command.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*command.Command(nil), s.cmds...)
}

func startFakeServer(t *testing.T, s api2.BridgeServiceServer) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	grpcServer := grpc.NewServer()
	api2.RegisterBridgeServiceServer(grpcServer, s)

	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.Stop)

	return lis.Addr().String()
}

func sensorDevice(id string, motionDetected, discharging bool) *device.Device {
	return &device.Device{
		Id: id,
		Details: &device.Device_Sensor{Sensor: &device.Sensor{
			Presence: &trait.Presence{State: &trait.Presence_State{MotionDetected: motionDetected}},
			Battery:  &trait.Battery{State: &trait.Battery_State{Discharging: discharging}},
		}},
	}
}

func lightDevice(id string, on bool) *device.Device {
	return &device.Device{
		Id: id,
		Details: &device.Device_Light{Light: &device.Light{
			OnOff: &trait.OnOff{State: &trait.OnOff_State{IsOn: on}},
		}},
	}
}

func colourLightDevice(id string, red int32) *device.Device {
	return &device.Device{
		Id: id,
		Details: &device.Device_Light{Light: &device.Light{
			OnOff:  &trait.OnOff{State: &trait.OnOff_State{}},
			Colour: &trait.Colour{State: &trait.Colour_State{Rgb: &trait.Colour_State_RGB{Red: red}}},
		}},
	}
}

func cameraDevice(id string, motionDetected bool) *device.Device {
	return &device.Device{
		Id: id,
		Details: &device.Device_Camera{Camera: &device.Camera{
			Presence: &trait.Presence{State: &trait.Presence_State{MotionDetected: motionDetected}},
		}},
	}
}

func waterSensorDevice(id string, active bool) *device.Device {
	return &device.Device{
		Id: id,
		Details: &device.Device_Sensor{Sensor: &device.Sensor{
			Water:    &device.Sensor_BinarySensor{IsActive: active},
			Metadata: &device.Sensor_Metadata{LowBattery: true},
		}},
	}
}

// initialAsBulk builds the INITIAL shape an individual bridge's own server
// sends: one Update carrying every device at once (service/bridge/api.go).
func initialAsBulk(bridgeID string, devices []*device.Device) []*api2.Update {
	return []*api2.Update{{
		Action: api2.Update_INITIAL,
		Update: &api2.Update_InitialUpdate{InitialUpdate: &api2.InitialUpdate{
			Bridge:  &api2.Bridge{Id: bridgeID, IsReachable: true},
			Devices: devices,
		}},
	}}
}

// initialAsPerDevice builds the INITIAL shape service/bridge/facade currently
// synthesizes instead: one DeviceUpdate per device, each individually marked
// Action: INITIAL.
func initialAsPerDevice(bridgeID string, devices []*device.Device) []*api2.Update {
	out := make([]*api2.Update, len(devices))
	for i, d := range devices {
		out[i] = &api2.Update{
			Action: api2.Update_INITIAL,
			Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
				DeviceId: d.GetId(),
				BridgeId: bridgeID,
				Device:   d,
			}},
		}
	}
	return out
}

func changedUpdate(bridgeID string, d *device.Device) *api2.Update {
	return &api2.Update{
		Action: api2.Update_CHANGED,
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
			DeviceId: d.GetId(),
			BridgeId: bridgeID,
			Device:   d,
		}},
	}
}

func TestResolveState(t *testing.T) {
	sensor := sensorDevice("sensor-1", true, false)
	light := colourLightDevice("light-1", 200)
	water := waterSensorDevice("water-1", true)

	for _, tc := range []struct {
		name    string
		device  *device.Device
		key     string
		want    any
		wantErr bool
	}{
		{"trait-shaped path", sensor, "presence.state.motion_detected", true, false},
		{"multi-segment trait path", light, "colour.state.rgb.red", int64(200), false},
		{"BinarySensor path, no .state hop", water, "water.is_active", true, false},
		{"Metadata path, no .state hop", water, "metadata.low_battery", true, false},
		{"unknown top-level segment", sensor, "no_such_trait.state.value", nil, true},
		{"unknown leaf segment", sensor, "presence.state.no_such_field", nil, true},
		{"path ends on a message, not a scalar", sensor, "battery", nil, true},
		{"path continues past a scalar", sensor, "presence.state.motion_detected.extra", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveState(tc.device, tc.key)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestAdapter_StreamedUpdatesDriveCacheAndSystemEvents runs the same
// assertions against two differently-shaped upstreams - one sending a bulk
// InitialUpdate (what a raw individual bridge sends) and one sending
// per-device DeviceUpdates marked INITIAL (what facade currently sends) - to
// prove Adapter treats both as equally expected, not one primary path and one
// fallback.
func TestAdapter_StreamedUpdatesDriveCacheAndSystemEvents(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial func(bridgeID string, devices []*device.Device) []*api2.Update
	}{
		{"bulk InitialUpdate (raw bridge shape)", initialAsBulk},
		{"per-device DeviceUpdate (facade shape)", initialAsPerDevice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No motion yet; battery discharging, i.e. currently running on
			// battery ("power out").
			sensor0 := sensorDevice("sensor-1", false, true)
			light0 := lightDevice("light-1", false)

			updates := tc.initial("b1", []*device.Device{sensor0, light0})
			updates = append(updates,
				changedUpdate("b1", sensorDevice("sensor-1", true, true)),  // motion rises
				changedUpdate("b1", sensorDevice("sensor-1", true, false)), // power restored
			)

			addr := startFakeServer(t, &fakeBridgeServer{updates: updates})

			adapter := New(zaptest.NewLogger(t), addr)
			engine := policy.NewEngine(adapter, policy.NewConditionRegistry(), zaptest.NewLogger(t))
			defer engine.Close()

			motionCh := engine.Bus().Subscribe("motion.detected")
			powerCh := engine.Bus().Subscribe("power.restored")

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			adapter.Start(ctx, engine)

			require.Eventually(t, func() bool {
				v, ok := engine.GetLastKnown("sensor-1")
				d, _ := v.(*device.Device)
				return ok && !d.GetSensor().GetBattery().GetState().GetDischarging()
			}, 2*time.Second, 10*time.Millisecond, "cache should reflect the final CHANGED update")

			select {
			case ev := <-motionCh:
				assert.Equal(t, "motion.detected", ev.Topic)
			case <-time.After(time.Second):
				t.Fatal("expected a motion.detected event on the rising edge")
			}
			select {
			case ev := <-powerCh:
				assert.Equal(t, "power.restored", ev.Topic)
			case <-time.After(time.Second):
				t.Fatal("expected a power.restored event on the falling edge")
			}

			select {
			case ev := <-motionCh:
				t.Fatalf("unexpected extra motion.detected event: %+v", ev)
			default:
			}
			select {
			case ev := <-powerCh:
				t.Fatalf("unexpected extra power.restored event: %+v", ev)
			default:
			}

			lv, ok := engine.GetLastKnown("light-1")
			require.True(t, ok)
			assert.False(t, lv.(*device.Device).GetLight().GetOnOff().GetState().GetIsOn())
		})
	}
}

// TestAdapter_NilDeviceOnNonRemovedUpdateDoesNotClobberCache guards against a
// bug where a DeviceUpdate with no Device (legal per bridge.proto for any
// Action other than REMOVED) overwrote the cached device with a typed-nil,
// silently zeroing its last-known state.
func TestAdapter_NilDeviceOnNonRemovedUpdateDoesNotClobberCache(t *testing.T) {
	updates := initialAsBulk("b1", []*device.Device{lightDevice("light-1", true)})
	updates = append(updates, &api2.Update{
		Action: api2.Update_EXECUTED,
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
			DeviceId: "light-1",
			BridgeId: "b1",
			// Device deliberately unset.
		}},
	})

	addr := startFakeServer(t, &fakeBridgeServer{updates: updates})

	adapter := New(zaptest.NewLogger(t), addr)
	engine := policy.NewEngine(adapter, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter.Start(ctx, engine)

	require.Eventually(t, func() bool {
		_, ok := engine.GetLastKnown("light-1")
		return ok
	}, 2*time.Second, 10*time.Millisecond)

	// Give the nil-Device update time to be processed too, then confirm the
	// cached state is still the real device, not nil.
	time.Sleep(50 * time.Millisecond)

	v, ok := engine.GetLastKnown("light-1")
	require.True(t, ok)
	d, ok := v.(*device.Device)
	require.True(t, ok, "cached value must still be the original *device.Device, not overwritten with nil")
	assert.True(t, d.GetLight().GetOnOff().GetState().GetIsOn())
}

// TestAdapter_NoMotionEventOnColdStartWithMotionAlreadyActive guards against
// treating a device's very first observed state as a motion rising edge: if
// a sensor's INITIAL update already reports motion active, that's the
// device's starting state, not a new transition, and must not publish
// motion.detected.
func TestAdapter_NoMotionEventOnColdStartWithMotionAlreadyActive(t *testing.T) {
	updates := initialAsBulk("b1", []*device.Device{sensorDevice("sensor-1", true, false)})
	addr := startFakeServer(t, &fakeBridgeServer{updates: updates})

	adapter := New(zaptest.NewLogger(t), addr)
	engine := policy.NewEngine(adapter, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	motionCh := engine.Bus().Subscribe("motion.detected")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter.Start(ctx, engine)

	require.Eventually(t, func() bool {
		_, ok := engine.GetLastKnown("sensor-1")
		return ok
	}, 2*time.Second, 10*time.Millisecond)

	select {
	case ev := <-motionCh:
		t.Fatalf("unexpected motion.detected event on cold start: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAdapter_SetLight(t *testing.T) {
	srv := &fakeBridgeServer{
		updates: initialAsBulk("b1", []*device.Device{lightDevice("light-1", false)}),
		cmdResp: lightDevice("light-1", true),
	}
	addr := startFakeServer(t, srv)

	adapter := New(zaptest.NewLogger(t), addr)
	engine := policy.NewEngine(adapter, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter.Start(ctx, engine)

	require.Eventually(t, func() bool {
		_, ok := engine.GetLastKnown("light-1")
		return ok
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, adapter.SetLight("light-1", true))

	cmds := srv.recordedCommands()
	require.Len(t, cmds, 1)
	assert.Equal(t, "light-1", cmds[0].GetDeviceId())
	require.NotNil(t, cmds[0].GetOnOff())
	assert.True(t, cmds[0].GetOnOff().GetOn())

	v, ok := engine.GetLastKnown("light-1")
	require.True(t, ok, "SetLight should apply the RPC's response immediately, before any stream update")
	assert.True(t, v.(*device.Device).GetLight().GetOnOff().GetState().GetIsOn())
}

func TestAdapter_SetLight_PropagatesError(t *testing.T) {
	srv := &fakeBridgeServer{
		updates: initialAsBulk("b1", nil),
		cmdErr:  status.Error(codes.Unavailable, "simulated: owning bridge unreachable"),
	}
	addr := startFakeServer(t, srv)

	adapter := New(zaptest.NewLogger(t), addr)
	engine := policy.NewEngine(adapter, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter.Start(ctx, engine)

	require.Eventually(t, func() bool {
		return adapter.conn.Client() != nil
	}, 2*time.Second, 10*time.Millisecond)

	err := adapter.SetLight("light-1", true)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code())
}

// newStartedAdapter is a small helper for the SetState tests below: it
// starts an Adapter against srv and waits for its connection to come up, so
// each test can focus on the SetState call itself.
func newStartedAdapter(t *testing.T, srv *fakeBridgeServer) (*Adapter, *policy.Engine) {
	t.Helper()

	addr := startFakeServer(t, srv)
	adapter := New(zaptest.NewLogger(t), addr)
	engine := policy.NewEngine(adapter, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	t.Cleanup(engine.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	adapter.Start(ctx, engine)

	require.Eventually(t, func() bool {
		return adapter.conn.Client() != nil
	}, 2*time.Second, 10*time.Millisecond)

	return adapter, engine
}

func TestAdapter_SetState_ScalarField(t *testing.T) {
	srv := &fakeBridgeServer{cmdResp: lightDevice("light-1", true)}
	adapter, _ := newStartedAdapter(t, srv)

	require.NoError(t, adapter.SetState("light-1", "brightness_absolute", map[string]any{
		"brightness_percent": float64(75),
	}))

	cmds := srv.recordedCommands()
	require.Len(t, cmds, 1)
	assert.Equal(t, "light-1", cmds[0].GetDeviceId())
	require.NotNil(t, cmds[0].GetBrightnessAbsolute())
	assert.EqualValues(t, 75, cmds[0].GetBrightnessAbsolute().GetBrightnessPercent())
}

func TestAdapter_SetState_NestedMessageField(t *testing.T) {
	srv := &fakeBridgeServer{cmdResp: lightDevice("light-1", true)}
	adapter, _ := newStartedAdapter(t, srv)

	require.NoError(t, adapter.SetState("light-1", "colour", map[string]any{
		"rgb": map[string]any{
			"red":   float64(255),
			"green": float64(10),
			"blue":  float64(0),
		},
	}))

	cmds := srv.recordedCommands()
	require.Len(t, cmds, 1)
	rgb := cmds[0].GetColour().GetRgb()
	require.NotNil(t, rgb)
	assert.EqualValues(t, 255, rgb.GetRed())
	assert.EqualValues(t, 10, rgb.GetGreen())
	assert.EqualValues(t, 0, rgb.GetBlue())
}

func TestAdapter_SetState_EnumField(t *testing.T) {
	srv := &fakeBridgeServer{cmdResp: lightDevice("thermo-1", true)}
	adapter, _ := newStartedAdapter(t, srv)

	require.NoError(t, adapter.SetState("thermo-1", "set_thermostat_mode", map[string]any{
		"mode": "HEAT",
	}))

	cmds := srv.recordedCommands()
	require.Len(t, cmds, 1)
	require.NotNil(t, cmds[0].GetSetThermostatMode())
	assert.Equal(t, trait.Thermostat_HEAT, cmds[0].GetSetThermostatMode().GetMode())
}

func TestAdapter_SetState_MapField(t *testing.T) {
	srv := &fakeBridgeServer{cmdResp: lightDevice("switch-1", true)}
	adapter, _ := newStartedAdapter(t, srv)

	require.NoError(t, adapter.SetState("switch-1", "toggle", map[string]any{
		"settings": map[string]any{
			"child_lock": true,
		},
	}))

	cmds := srv.recordedCommands()
	require.Len(t, cmds, 1)
	assert.Equal(t, map[string]bool{"child_lock": true}, cmds[0].GetToggle().GetSettings())
}

func TestAdapter_SetState_UnknownCommand(t *testing.T) {
	srv := &fakeBridgeServer{}
	adapter, _ := newStartedAdapter(t, srv)

	err := adapter.SetState("light-1", "not_a_real_command", map[string]any{})
	require.Error(t, err)
	assert.Empty(t, srv.recordedCommands())
}

// TestAdapter_SetState_RejectsNonDetailsFields checks that key can't be
// used to smuggle a write to Command's own device_id/id/version fields -
// only Command.details' oneof members are valid commands.
func TestAdapter_SetState_RejectsNonDetailsFields(t *testing.T) {
	srv := &fakeBridgeServer{}
	adapter, _ := newStartedAdapter(t, srv)

	err := adapter.SetState("light-1", "device_id", map[string]any{})
	require.Error(t, err)
	assert.Empty(t, srv.recordedCommands())
}

func TestAdapter_SetState_ValueMustBeATable(t *testing.T) {
	srv := &fakeBridgeServer{}
	adapter, _ := newStartedAdapter(t, srv)

	err := adapter.SetState("light-1", "brightness_absolute", 75)
	require.Error(t, err)
	assert.Empty(t, srv.recordedCommands())
}

func TestAdapter_SetState_ErrNotReadyBeforeStart(t *testing.T) {
	adapter := New(zaptest.NewLogger(t), "127.0.0.1:0")
	err := adapter.SetState("light-1", "brightness_absolute", map[string]any{"brightness_percent": float64(50)})
	assert.ErrorIs(t, err, ErrNotReady)
}

// TestHasMotionCoversAnyDeviceKindWithAPresenceField is finding C's
// regression test: hasMotion must not be hardcoded to Sensor - Camera (and
// any other kind whose details message has a "presence" field) has to work
// too, since it now goes through the same generic resolveState path
// GetState uses.
func TestHasMotionCoversAnyDeviceKindWithAPresenceField(t *testing.T) {
	assert.True(t, hasMotion(sensorDevice("sensor-1", true, false)))
	assert.False(t, hasMotion(sensorDevice("sensor-1", false, false)))
	assert.True(t, hasMotion(cameraDevice("camera-1", true)))
	assert.False(t, hasMotion(cameraDevice("camera-1", false)))
	assert.False(t, hasMotion(lightDevice("light-1", true)), "a kind with no presence field must not panic or false-positive")
}

func TestDeviceKind(t *testing.T) {
	assert.Equal(t, "sensor", deviceKind(sensorDevice("sensor-1", false, false)))
	assert.Equal(t, "light", deviceKind(lightDevice("light-1", false)))
	assert.Equal(t, "camera", deviceKind(cameraDevice("camera-1", false)))
	assert.Equal(t, "", deviceKind(&device.Device{Id: "no-details"}))
}

// TestAdapter_CameraMotionPublishesMotionDetected proves finding C end to
// end: a camera-sourced motion rising edge now reaches the shared
// "motion.detected" bus topic the same way a Sensor's does.
func TestAdapter_CameraMotionPublishesMotionDetected(t *testing.T) {
	updates := initialAsBulk("b1", []*device.Device{cameraDevice("camera-1", false)})
	updates = append(updates, changedUpdate("b1", cameraDevice("camera-1", true)))

	addr := startFakeServer(t, &fakeBridgeServer{updates: updates})

	adapter := New(zaptest.NewLogger(t), addr)
	engine := policy.NewEngine(adapter, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	motionCh := engine.Bus().Subscribe("motion.detected")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter.Start(ctx, engine)

	select {
	case ev := <-motionCh:
		assert.Equal(t, "motion.detected", ev.Topic)
	case <-time.After(2 * time.Second):
		t.Fatal("expected a motion.detected event for camera-sourced motion")
	}
}

// TestAdapter_UpdateDeviceStateTagsEngineCacheWithKind proves devices
// streamed through the Adapter land in the engine's cache tagged with their
// kind, so Engine.DevicesOfKind (and policy scripts' home.findDevices) can
// enumerate them without any HomeAPI support for listing devices.
func TestAdapter_UpdateDeviceStateTagsEngineCacheWithKind(t *testing.T) {
	updates := initialAsBulk("b1", []*device.Device{
		lightDevice("light-1", false),
		sensorDevice("sensor-1", false, false),
		cameraDevice("camera-1", false),
	})
	addr := startFakeServer(t, &fakeBridgeServer{updates: updates})

	adapter := New(zaptest.NewLogger(t), addr)
	engine := policy.NewEngine(adapter, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter.Start(ctx, engine)

	require.Eventually(t, func() bool {
		return len(engine.DevicesOfKind("light")) == 1 &&
			len(engine.DevicesOfKind("sensor")) == 1 &&
			len(engine.DevicesOfKind("camera")) == 1
	}, 2*time.Second, 10*time.Millisecond)

	assert.Equal(t, []string{"light-1"}, engine.DevicesOfKind("light"))
	assert.Empty(t, engine.DevicesOfKind("ups"))
}

func TestAdapter_ErrNotReadyBeforeStart(t *testing.T) {
	adapter := New(zaptest.NewLogger(t), "unused:0")

	_, err := adapter.GetLight("x")
	assert.ErrorIs(t, err, ErrNotReady)

	assert.ErrorIs(t, adapter.SetLight("x", true), ErrNotReady)

	_, err = adapter.GetLastKnown("x")
	assert.ErrorIs(t, err, ErrNotReady)
}
