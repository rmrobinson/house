package house

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/house/db"
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

// A timer that fires at exactly the grace deadline (not a moment after) must
// still publish the vacant transition.
func TestPresence_TimerFiringExactlyAtDeadlinePublishesVacant(t *testing.T) {
	h := newPresenceHarness(t)
	h.a.setPresenceTracked("phone", true)
	h.a.handleUpdate(phoneUpdate("phone", true))
	h.a.handleUpdate(phoneUpdate("phone", false))
	require.Len(t, h.timer, 1)

	sink := h.a.updates.NewSink()
	defer sink.Close()
	h.now = h.now.Add(DefaultPresenceGrace)
	h.timer[0]()

	select {
	case msg := <-sink.Messages():
		assert.False(t, msg.(*api2.HouseUpdate).GetBuilding().GetState().GetOccupied())
	case <-time.After(time.Second):
		t.Fatal("no vacant update at the exact grace deadline")
	}
}

// Un-tracking one phone must not cut short the grace period another phone's
// departure already started.
func TestPresence_UntrackingOnePhoneKeepsOthersGrace(t *testing.T) {
	h := newPresenceHarness(t)
	h.a.setPresenceTracked("todd", true)
	h.a.setPresenceTracked("robert", true)
	h.a.handleUpdate(phoneUpdate("todd", true))
	h.a.handleUpdate(phoneUpdate("robert", true))
	h.a.handleUpdate(phoneUpdate("todd", false))
	h.a.handleUpdate(phoneUpdate("robert", false)) // grace starts here

	h.now = h.now.Add(5 * time.Minute)
	h.a.setPresenceTracked("todd", false)

	got := occupied(h.a)
	require.NotNil(t, got)
	assert.True(t, *got, "robert's grace period is still running")
}

func TestPresence_StartupWithTrackedDevicesGetsGrace(t *testing.T) {
	d := newPresenceTestDB(t)
	_, err := d.SetDeviceConfig(context.Background(), db.DeviceConfig{ID: "phone", Name: "Pixel", PresenceTracked: true})
	require.NoError(t, err)
	b, err := d.CreateBuilding(context.Background(), &db.Building{Name: "h", TZ: "America/Toronto"})
	require.NoError(t, err)

	a := newAggregator(zaptest.NewLogger(t))
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }
	var timers int
	a.afterFunc = func(time.Duration, func()) *time.Timer { timers++; return nil }
	require.NoError(t, a.load(context.Background(), d))
	assert.Equal(t, 1, timers, "a grace timer must be armed at startup")

	// Phone is first reported already unreachable (asleep): still occupied
	// for the grace period, then vacant.
	a.handleUpdate(phoneUpdate("phone", false))
	got := a.buildingOccupied(b.ID)
	require.NotNil(t, got)
	assert.True(t, *got)
	now = now.Add(DefaultPresenceGrace + time.Second)
	got = a.buildingOccupied(b.ID)
	require.NotNil(t, got)
	assert.False(t, *got)
}

func TestPresence_RemovedDeviceStopsCounting(t *testing.T) {
	h := newPresenceHarness(t)
	h.a.setPresenceTracked("phone", true)
	h.a.handleUpdate(phoneUpdate("phone", true))

	rm := phoneUpdate("phone", true)
	rm.Action = api2.Update_REMOVED
	h.a.handleUpdate(rm)
	require.Len(t, h.timer, 1, "removal of the last reachable tracked device starts grace")

	h.now = h.now.Add(DefaultPresenceGrace + time.Second)
	got := occupied(h.a)
	require.NotNil(t, got)
	assert.False(t, *got)
}

func TestPresence_UpdateWithoutAddressIsIgnored(t *testing.T) {
	h := newPresenceHarness(t)
	h.a.setPresenceTracked("phone", true)
	h.a.handleUpdate(phoneUpdate("phone", true))

	u := phoneUpdate("phone", true)
	u.GetDeviceUpdate().Device.Address = nil
	h.a.handleUpdate(u)

	assert.Empty(t, h.timer)
	got := occupied(h.a)
	require.NotNil(t, got)
	assert.True(t, *got)
}

func newPresenceTestDB(t *testing.T) *db.Database {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	d, err := db.NewDatabase(zaptest.NewLogger(t), sqlDB)
	require.NoError(t, err)
	return d
}
