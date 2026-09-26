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

func TestAdapter_ErrNotReadyBeforeStart(t *testing.T) {
	adapter := New(zaptest.NewLogger(t), "unused:0")

	_, err := adapter.GetLight("x")
	assert.ErrorIs(t, err, ErrNotReady)

	assert.ErrorIs(t, adapter.SetLight("x", true), ErrNotReady)

	_, err = adapter.GetLastKnown("x")
	assert.ErrorIs(t, err, ErrNotReady)
}
