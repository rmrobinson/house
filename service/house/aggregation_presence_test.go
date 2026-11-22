package house

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
)

func phoneUpdate(id string, reachable bool) *api2.Update {
	return &api2.Update{
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
			DeviceId: id,
			Device: &apiDevice.Device{
				Id:      id,
				Address: &apiDevice.Device_Address{IsReachable: reachable},
				Details: &apiDevice.Device_ConnectedDevice{ConnectedDevice: &apiDevice.ConnectedDevice{}},
			},
		}},
	}
}

// presenceHarness builds an aggregator with a controllable clock and a
// captured (not auto-fired) afterFunc, with one building and no rooms.
type presenceHarness struct {
	a     *aggregator
	now   time.Time
	timer []func()
}

func newPresenceHarness(t *testing.T) *presenceHarness {
	h := &presenceHarness{now: time.Unix(1_700_000_000, 0)}
	h.a = newAggregator(zaptest.NewLogger(t))
	h.a.now = func() time.Time { return h.now }
	h.a.afterFunc = func(d time.Duration, f func()) *time.Timer {
		assert.Equal(t, DefaultPresenceGrace, d)
		h.timer = append(h.timer, f)
		return nil
	}
	h.a.registerBuilding("b1")
	return h
}

func occupied(a *aggregator) *bool { return a.buildingOccupied("b1") }

func TestPresence_NoTrackedDevicesIsUnknown(t *testing.T) {
	h := newPresenceHarness(t)
	h.a.handleUpdate(phoneUpdate("phone", true))
	assert.Nil(t, occupied(h.a), "an untracked device must not produce an occupancy signal")
}

func TestPresence_ReachableTrackedDeviceKeepsBuildingOccupied(t *testing.T) {
	h := newPresenceHarness(t)
	h.a.setPresenceTracked("phone", true)
	h.a.handleUpdate(phoneUpdate("phone", true))

	got := occupied(h.a)
	require.NotNil(t, got)
	assert.True(t, *got)

	// Stays occupied for as long as it's reachable, however much time passes
	// with no motion at all.
	h.now = h.now.Add(10 * time.Hour)
	got = occupied(h.a)
	require.NotNil(t, got)
	assert.True(t, *got)
}

func TestPresence_GraceThenVacant(t *testing.T) {
	h := newPresenceHarness(t)
	h.a.setPresenceTracked("phone", true)
	h.a.handleUpdate(phoneUpdate("phone", true))

	sink := h.a.updates.NewSink()
	defer sink.Close()

	h.a.handleUpdate(phoneUpdate("phone", false))
	require.Len(t, h.timer, 1, "losing the last tracked device must arm the grace timer")

	// Within grace: still occupied, and nothing published.
	h.now = h.now.Add(DefaultPresenceGrace - time.Minute)
	got := occupied(h.a)
	require.NotNil(t, got)
	assert.True(t, *got)
	select {
	case msg := <-sink.Messages():
		t.Fatalf("unexpected update inside the grace period: %v", msg)
	default:
	}

	// Past grace: the timer firing publishes occupied=false.
	h.now = h.now.Add(2 * time.Minute)
	h.timer[0]()
	select {
	case msg := <-sink.Messages():
		bu := msg.(*api2.HouseUpdate).GetBuilding()
		require.NotNil(t, bu)
		require.NotNil(t, bu.GetState().Occupied)
		assert.False(t, bu.GetState().GetOccupied())
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the vacant update")
	}
}

func TestPresence_ReturnWithinGraceStaysOccupied(t *testing.T) {
	h := newPresenceHarness(t)
	h.a.setPresenceTracked("phone", true)
	h.a.handleUpdate(phoneUpdate("phone", true))
	h.a.handleUpdate(phoneUpdate("phone", false))

	h.now = h.now.Add(5 * time.Minute)
	h.a.handleUpdate(phoneUpdate("phone", true))

	// Even once the original grace deadline passes (and its stale timer
	// fires), the returned phone keeps the building occupied.
	h.now = h.now.Add(DefaultPresenceGrace)
	h.timer[0]()
	got := occupied(h.a)
	require.NotNil(t, got)
	assert.True(t, *got)
}

func TestPresence_AnyOfSeveralPhonesHoldsOccupancy(t *testing.T) {
	h := newPresenceHarness(t)
	h.a.setPresenceTracked("todd", true)
	h.a.setPresenceTracked("robert", true)
	h.a.handleUpdate(phoneUpdate("todd", true))
	h.a.handleUpdate(phoneUpdate("robert", true))

	h.a.handleUpdate(phoneUpdate("todd", false))
	assert.Empty(t, h.timer, "grace must not start while another tracked device is still reachable")

	h.now = h.now.Add(3 * DefaultPresenceGrace)
	got := occupied(h.a)
	require.NotNil(t, got)
	assert.True(t, *got, "robert's phone is still reachable")

	h.a.handleUpdate(phoneUpdate("robert", false))
	require.Len(t, h.timer, 1)
	h.now = h.now.Add(DefaultPresenceGrace + time.Second)
	got = occupied(h.a)
	require.NotNil(t, got)
	assert.False(t, *got)
}

func TestPresence_MotionStillCountsAfterPhonesLeave(t *testing.T) {
	h := newPresenceHarness(t)
	h.a.registerRoom("room-1", "b1")
	h.a.setDeviceRoom("sensor-1", "room-1")
	h.a.setPresenceTracked("phone", true)
	h.a.handleUpdate(phoneUpdate("phone", true))
	h.a.handleUpdate(phoneUpdate("phone", false))

	// Grace has long lapsed, but a guest trips the motion sensor.
	h.now = h.now.Add(2 * DefaultPresenceGrace)
	h.a.afterFunc = func(time.Duration, func()) *time.Timer { return nil }
	h.a.handleUpdate(motionUpdate("sensor-1", true))
	got := occupied(h.a)
	require.NotNil(t, got)
	assert.True(t, *got)

	// ...and once the motion window lapses too, the house is vacant.
	h.now = h.now.Add(buildingOccupiedWindow + time.Second)
	got = occupied(h.a)
	require.NotNil(t, got)
	assert.False(t, *got)
}

func TestPresence_FlaggingAnAlreadyReachableDeviceTakesEffectImmediately(t *testing.T) {
	h := newPresenceHarness(t)
	h.a.handleUpdate(phoneUpdate("phone", true)) // seen before being flagged
	assert.Nil(t, occupied(h.a))

	sink := h.a.updates.NewSink()
	defer sink.Close()
	h.a.setPresenceTracked("phone", true)

	select {
	case msg := <-sink.Messages():
		assert.True(t, msg.(*api2.HouseUpdate).GetBuilding().GetState().GetOccupied())
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the occupied update")
	}
}

func TestPresence_UnflaggingDropsItImmediatelyWithoutGrace(t *testing.T) {
	h := newPresenceHarness(t)
	h.a.setPresenceTracked("phone", true)
	h.a.handleUpdate(phoneUpdate("phone", true))

	h.a.setPresenceTracked("phone", false)
	// No tracked devices remain, so there's no presence signal any more -
	// not a grace period's worth of lingering "occupied".
	assert.Nil(t, occupied(h.a))
	assert.Empty(t, h.timer)
}

func TestPresence_TrackedDeviceNeverSeenIsNotASignal(t *testing.T) {
	h := newPresenceHarness(t)
	h.a.setPresenceTracked("phone", true)
	assert.Nil(t, occupied(h.a), "no reading yet is unknown, not vacant")
}
