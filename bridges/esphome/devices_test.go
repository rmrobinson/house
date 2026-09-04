package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	esphome "github.com/richard87/esphome-apiclient"
	"github.com/richard87/esphome-apiclient/pb"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

// --- numeric helpers ---

func TestBrightnessToPercent(t *testing.T) {
	tests := []struct {
		name string
		in   float32
		want int32
	}{
		{"off", 0.0, 0},
		{"full", 1.0, 100},
		{"quarter", 0.25, 25},
		{"half", 0.5, 50},
		{"rounds up", 0.429, 43},
		{"rounds down", 0.421, 42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, brightnessToPercent(tt.in))
		})
	}
}

func TestPercentToBrightness(t *testing.T) {
	assert.InDelta(t, 0.0, percentToBrightness(0), 0.001)
	assert.InDelta(t, 1.0, percentToBrightness(100), 0.001)
	assert.InDelta(t, 0.42, percentToBrightness(42), 0.001)
}

func TestBrightnessRoundTrip(t *testing.T) {
	for pct := int32(0); pct <= 100; pct++ {
		assert.Equal(t, pct, brightnessToPercent(percentToBrightness(pct)), "pct=%d", pct)
	}
}

func TestClampPercent(t *testing.T) {
	tests := []struct {
		in, want int32
	}{
		{-50, 0}, {-1, 0}, {0, 0}, {50, 50}, {100, 100}, {101, 100}, {1000, 100},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, clampPercent(tt.in), "in=%d", tt.in)
	}
}

// --- requireLightEntity ---

func TestRequireLightEntity(t *testing.T) {
	t.Run("missing role", func(t *testing.T) {
		_, err := requireLightEntity(map[string]esphome.Entity{})
		assert.Error(t, err)
	})

	t.Run("wrong entity type", func(t *testing.T) {
		_, err := requireLightEntity(map[string]esphome.Entity{
			"light": &esphome.SwitchEntity{Key: 1, ObjectID: "not_a_light"},
		})
		assert.Error(t, err)
	})

	t.Run("correct type", func(t *testing.T) {
		le := &esphome.LightEntity{Key: 42, ObjectID: "office_lamp", State: true, Brightness: 0.5}
		got, err := requireLightEntity(map[string]esphome.Entity{"light": le})
		require.NoError(t, err)
		assert.Same(t, le, got)
	})
}

// --- lightBuilder.build ---

func TestLightBuilder_Build(t *testing.T) {
	t.Run("missing role errors", func(t *testing.T) {
		_, err := lightBuilder{}.build(deviceConfig{ID: "l1", Type: "light"}, map[string]esphome.Entity{})
		assert.Error(t, err)
	})

	t.Run("populates OnOff and Brightness from the entity", func(t *testing.T) {
		entities := map[string]esphome.Entity{
			"light": &esphome.LightEntity{Key: 1, ObjectID: "office_lamp", State: true, Brightness: 0.42},
		}
		d, err := lightBuilder{}.build(deviceConfig{ID: "office-lamp", Type: "light"}, entities)
		require.NoError(t, err)

		require.NotNil(t, d.GetLight())
		assert.Equal(t, "ESPHome", d.Manufacturer)
		assert.True(t, d.GetLight().GetOnOff().GetAttributes().GetCanControl())
		assert.True(t, d.GetLight().GetOnOff().GetState().GetIsOn())
		assert.True(t, d.GetLight().GetBrightness().GetAttributes().GetCanControl())
		assert.EqualValues(t, 42, d.GetLight().GetBrightness().GetState().GetLevel())
	})

	t.Run("off state carries through", func(t *testing.T) {
		entities := map[string]esphome.Entity{
			"light": &esphome.LightEntity{Key: 1, ObjectID: "office_lamp", State: false, Brightness: 0},
		}
		d, err := lightBuilder{}.build(deviceConfig{ID: "office-lamp", Type: "light"}, entities)
		require.NoError(t, err)
		assert.False(t, d.GetLight().GetOnOff().GetState().GetIsOn())
		assert.EqualValues(t, 0, d.GetLight().GetBrightness().GetState().GetLevel())
	})
}

// --- clockBuilder.build ---

func TestClockBuilder_Build(t *testing.T) {
	entities := func() map[string]esphome.Entity {
		return map[string]esphome.Entity{
			"light": &esphome.LightEntity{Key: 1, ObjectID: "clock_display", State: true, Brightness: 1.0},
		}
	}

	t.Run("missing role errors", func(t *testing.T) {
		_, err := clockBuilder{}.build(deviceConfig{ID: "c1", Type: "clock"}, map[string]esphome.Entity{})
		assert.Error(t, err)
	})

	t.Run("defaults timezone to local and format to 24h", func(t *testing.T) {
		d, err := clockBuilder{}.build(deviceConfig{ID: "c1", Type: "clock"}, entities())
		require.NoError(t, err)

		require.NotNil(t, d.GetClock())
		assert.Equal(t, time.Local.String(), d.GetClock().GetTime().GetState().GetTimezone())
		assert.Equal(t, trait.Time_TIME_FORMAT_24H, d.GetClock().GetTime().GetState().GetTimeFormat())
		assert.True(t, d.GetClock().GetOnOff().GetState().GetIsOn())
		assert.EqualValues(t, 100, d.GetClock().GetBrightness().GetState().GetLevel())
		assert.NotNil(t, d.GetClock().GetTime().GetState().GetUtc())
		assert.NotEmpty(t, d.GetClock().GetTime().GetState().GetLocalTime())
	})

	t.Run("honours configured timezone and 12h format", func(t *testing.T) {
		d, err := clockBuilder{}.build(deviceConfig{
			ID: "c1", Type: "clock", Timezone: "America/Toronto", TimeFormat: "12h",
		}, entities())
		require.NoError(t, err)

		assert.Equal(t, "America/Toronto", d.GetClock().GetTime().GetState().GetTimezone())
		assert.Equal(t, trait.Time_TIME_FORMAT_12H, d.GetClock().GetTime().GetState().GetTimeFormat())
	})

	t.Run("unrecognized time_format falls back to 24h", func(t *testing.T) {
		d, err := clockBuilder{}.build(deviceConfig{ID: "c1", Type: "clock", TimeFormat: "bogus"}, entities())
		require.NoError(t, err)
		assert.Equal(t, trait.Time_TIME_FORMAT_24H, d.GetClock().GetTime().GetState().GetTimeFormat())
	})
}

// --- applyState ---

func TestLightBuilder_ApplyState(t *testing.T) {
	newDevice := func() *device.Device {
		d, err := lightBuilder{}.build(deviceConfig{ID: "l1", Type: "light"}, map[string]esphome.Entity{
			"light": &esphome.LightEntity{Key: 1, State: false, Brightness: 0},
		})
		require.NoError(t, err)
		return d
	}

	t.Run("updates on/off and brightness for the light role", func(t *testing.T) {
		d := newDevice()
		lightBuilder{}.applyState(d, "light", &pb.LightStateResponse{Key: 1, State: true, Brightness: 0.75})
		assert.True(t, d.GetLight().GetOnOff().GetState().GetIsOn())
		assert.EqualValues(t, 75, d.GetLight().GetBrightness().GetState().GetLevel())
	})

	t.Run("ignores a role that isn't light", func(t *testing.T) {
		d := newDevice()
		lightBuilder{}.applyState(d, "other", &pb.LightStateResponse{Key: 1, State: true, Brightness: 0.75})
		assert.False(t, d.GetLight().GetOnOff().GetState().GetIsOn())
		assert.EqualValues(t, 0, d.GetLight().GetBrightness().GetState().GetLevel())
	})

	t.Run("ignores a message of the wrong type", func(t *testing.T) {
		d := newDevice()
		lightBuilder{}.applyState(d, "light", &pb.SwitchStateResponse{Key: 1, State: true})
		assert.False(t, d.GetLight().GetOnOff().GetState().GetIsOn())
	})
}

func TestClockBuilder_ApplyState(t *testing.T) {
	newDevice := func() *device.Device {
		d, err := clockBuilder{}.build(deviceConfig{ID: "c1", Type: "clock", Timezone: "UTC"}, map[string]esphome.Entity{
			"light": &esphome.LightEntity{Key: 1, State: false, Brightness: 0},
		})
		require.NoError(t, err)
		return d
	}

	t.Run("updates on/off and brightness, leaves Time alone", func(t *testing.T) {
		d := newDevice()
		wantTZ := d.GetClock().GetTime().GetState().GetTimezone()

		clockBuilder{}.applyState(d, "light", &pb.LightStateResponse{Key: 1, State: true, Brightness: 0.3})

		assert.True(t, d.GetClock().GetOnOff().GetState().GetIsOn())
		assert.EqualValues(t, 30, d.GetClock().GetBrightness().GetState().GetLevel())
		assert.Equal(t, wantTZ, d.GetClock().GetTime().GetState().GetTimezone())
	})

	t.Run("ignores a role that isn't light", func(t *testing.T) {
		d := newDevice()
		clockBuilder{}.applyState(d, "other", &pb.LightStateResponse{Key: 1, State: true, Brightness: 0.3})
		assert.False(t, d.GetClock().GetOnOff().GetState().GetIsOn())
	})
}

// --- applyCommand: paths that return before touching the client ---

func TestLightBuilder_ApplyCommand_ErrorsWithoutTouchingClient(t *testing.T) {
	d, err := lightBuilder{}.build(deviceConfig{ID: "l1", Type: "light"}, map[string]esphome.Entity{
		"light": &esphome.LightEntity{Key: 1},
	})
	require.NoError(t, err)

	t.Run("missing role key", func(t *testing.T) {
		err := lightBuilder{}.applyCommand(nil, map[string]uint32{}, d, &command.Command{
			DeviceId: "l1",
			Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
		})
		assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
	})

	t.Run("unsupported command type", func(t *testing.T) {
		err := lightBuilder{}.applyCommand(nil, map[string]uint32{"light": 1}, d, &command.Command{DeviceId: "l1"})
		assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
	})
}

func TestClockBuilder_ApplyCommand_TimeCommandDoesNotTouchClient(t *testing.T) {
	d, err := clockBuilder{}.build(deviceConfig{ID: "c1", Type: "clock", Timezone: "UTC"}, map[string]esphome.Entity{
		"light": &esphome.LightEntity{Key: 1},
	})
	require.NoError(t, err)

	format := trait.Time_TIME_FORMAT_12H
	// A nil *esphome.Client is safe here specifically because the Time command path never
	// dereferences it — it's bridge-side bookkeeping only (see the clockBuilder doc comment in
	// devices.go: ESPHome has no wire message for pushing a time format to a node).
	err = clockBuilder{}.applyCommand(nil, map[string]uint32{"light": 1}, d, &command.Command{
		DeviceId: "c1",
		Details: &command.Command_Time{Time: &command.Time{
			Timezone: strPtr("America/Toronto"),
			Format:   &format,
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, "America/Toronto", d.GetClock().GetTime().GetState().GetTimezone())
	assert.Equal(t, trait.Time_TIME_FORMAT_12H, d.GetClock().GetTime().GetState().GetTimeFormat())
}

func TestClockBuilder_ApplyCommand_RejectsInvalidTimezone(t *testing.T) {
	d, err := clockBuilder{}.build(deviceConfig{ID: "c1", Type: "clock", Timezone: "UTC"}, map[string]esphome.Entity{
		"light": &esphome.LightEntity{Key: 1},
	})
	require.NoError(t, err)

	err = clockBuilder{}.applyCommand(nil, map[string]uint32{"light": 1}, d, &command.Command{
		DeviceId: "c1",
		Details:  &command.Command_Time{Time: &command.Time{Timezone: strPtr("Not/A_Real_Zone")}},
	})
	assert.ErrorIs(t, err, bridge.ErrInvalidTimezone)
	assert.Equal(t, "UTC", d.GetClock().GetTime().GetState().GetTimezone(), "rejected timezone must not be applied")
}

func TestClockBuilder_ApplyCommand_MissingRoleKey(t *testing.T) {
	d, err := clockBuilder{}.build(deviceConfig{ID: "c1", Type: "clock"}, map[string]esphome.Entity{
		"light": &esphome.LightEntity{Key: 1},
	})
	require.NoError(t, err)

	err = clockBuilder{}.applyCommand(nil, map[string]uint32{}, d, &command.Command{
		DeviceId: "c1",
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
}

// --- applyCommand: paths that send to the node ---
//
// OnOff/BrightnessAbsolute/BrightnessRelative all call client.SetLight, which needs a real,
// non-nil *esphome.Client — there's no interface seam in the vendored library to fake that out
// cheaply. These dial a real (loopback, plaintext) connection against the same fake ESPHome
// server used by the reconnect e2e test, but call the builder directly rather than going through
// nodeConn/EsphomeBridge, so they're still exercising just the builder's translation logic, not
// discovery/matching/routing.

func dialFakeServerClient(t *testing.T, server *fakeESPHomeServer) *esphome.Client {
	t.Helper()
	client, err := esphome.Dial(server.Addr(), 2*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	return client
}

func TestLightBuilder_ApplyCommand_SendsToNode(t *testing.T) {
	server := newFakeESPHomeServer(t, nil)
	client := dialFakeServerClient(t, server)

	d, err := lightBuilder{}.build(deviceConfig{ID: "l1", Type: "light"}, map[string]esphome.Entity{
		"light": &esphome.LightEntity{Key: fakeLightKey, State: false, Brightness: 0.9},
	})
	require.NoError(t, err)
	roleKeys := map[string]uint32{"light": fakeLightKey}

	t.Run("on/off", func(t *testing.T) {
		err := lightBuilder{}.applyCommand(client, roleKeys, d, &command.Command{
			DeviceId: "l1",
			Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
		})
		require.NoError(t, err)
		assert.True(t, d.GetLight().GetOnOff().GetState().GetIsOn())

		require.Eventually(t, func() bool { return len(server.Commands()) == 1 }, time.Second, 10*time.Millisecond)
		cmd := server.Commands()[0]
		assert.Equal(t, fakeLightKey, cmd.Key)
		assert.True(t, cmd.HasState)
		assert.True(t, cmd.State)
	})

	t.Run("brightness absolute", func(t *testing.T) {
		err := lightBuilder{}.applyCommand(client, roleKeys, d, &command.Command{
			DeviceId: "l1",
			Details:  &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: 20}},
		})
		require.NoError(t, err)
		assert.EqualValues(t, 20, d.GetLight().GetBrightness().GetState().GetLevel())

		require.Eventually(t, func() bool { return len(server.Commands()) == 2 }, time.Second, 10*time.Millisecond)
		cmd := server.Commands()[1]
		assert.True(t, cmd.HasBrightness)
		assert.InDelta(t, 0.2, cmd.Brightness, 0.001)
	})

	t.Run("brightness relative clamps at 100", func(t *testing.T) {
		// current level is 20 (from the previous subtest) + 90 would be 110, clamp to 100.
		err := lightBuilder{}.applyCommand(client, roleKeys, d, &command.Command{
			DeviceId: "l1",
			Details:  &command.Command_BrightnessRelative{BrightnessRelative: &command.BrightnessRelative{ChangePercent: 90}},
		})
		require.NoError(t, err)
		assert.EqualValues(t, 100, d.GetLight().GetBrightness().GetState().GetLevel())

		require.Eventually(t, func() bool { return len(server.Commands()) == 3 }, time.Second, 10*time.Millisecond)
		cmd := server.Commands()[2]
		assert.True(t, cmd.HasBrightness)
		assert.InDelta(t, 1.0, cmd.Brightness, 0.001)
	})
}

func TestClockBuilder_ApplyCommand_SendsToNode(t *testing.T) {
	server := newFakeESPHomeServer(t, nil)
	client := dialFakeServerClient(t, server)

	d, err := clockBuilder{}.build(deviceConfig{ID: "c1", Type: "clock"}, map[string]esphome.Entity{
		"light": &esphome.LightEntity{Key: fakeLightKey, State: false, Brightness: 0},
	})
	require.NoError(t, err)
	roleKeys := map[string]uint32{"light": fakeLightKey}

	err = clockBuilder{}.applyCommand(client, roleKeys, d, &command.Command{
		DeviceId: "c1",
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	require.NoError(t, err)
	assert.True(t, d.GetClock().GetOnOff().GetState().GetIsOn())

	require.Eventually(t, func() bool { return len(server.Commands()) == 1 }, time.Second, 10*time.Millisecond)
	cmd := server.Commands()[0]
	assert.Equal(t, fakeLightKey, cmd.Key)
	assert.True(t, cmd.HasState)
	assert.True(t, cmd.State)
}

func strPtr(s string) *string { return &s }

// --- fanBuilder ---

const (
	fakeFanKey     uint32 = 200001
	fakeModeKey    uint32 = 200002
	fakeTimerKey   uint32 = 200003
	fakeOscKey     uint32 = 200004
	fakeBeepKey    uint32 = 200005
	fakeDisplayKey uint32 = 200006
)

func fanEntities() map[string]esphome.Entity {
	return map[string]esphome.Entity{
		"fan":         &esphome.FanEntity{Key: fakeFanKey, State: false, SpeedLevel: 2, SupportedSpeedCount: 5},
		"mode":        &esphome.SelectEntity{Key: fakeModeKey, Options: []string{"Normal", "Natural", "Sleep"}, State: "Normal"},
		"timer":       &esphome.SelectEntity{Key: fakeTimerKey, Options: fanTimerLabels, State: "Cancel"},
		"oscillation": &esphome.SwitchEntity{Key: fakeOscKey, State: true},
		"beep":        &esphome.SwitchEntity{Key: fakeBeepKey, State: false},
	}
}

func TestFanBuilder_Build(t *testing.T) {
	t.Run("missing fan role errors", func(t *testing.T) {
		entities := fanEntities()
		delete(entities, "fan")
		_, err := fanBuilder{}.build(deviceConfig{ID: "f1", Type: "fan"}, entities)
		assert.Error(t, err)
	})

	t.Run("missing mode role errors", func(t *testing.T) {
		entities := fanEntities()
		delete(entities, "mode")
		_, err := fanBuilder{}.build(deviceConfig{ID: "f1", Type: "fan"}, entities)
		assert.Error(t, err)
	})

	t.Run("missing timer role errors", func(t *testing.T) {
		entities := fanEntities()
		delete(entities, "timer")
		_, err := fanBuilder{}.build(deviceConfig{ID: "f1", Type: "fan"}, entities)
		assert.Error(t, err)
	})

	t.Run("populates every trait from the matched entities", func(t *testing.T) {
		d, err := fanBuilder{}.build(deviceConfig{ID: "f1", Type: "fan"}, fanEntities())
		require.NoError(t, err)

		f := d.GetFan()
		require.NotNil(t, f)
		assert.Equal(t, "ESPHome", d.Manufacturer)

		assert.True(t, f.GetOnOff().GetAttributes().GetCanControl())
		assert.False(t, f.GetOnOff().GetState().GetIsOn())

		assert.EqualValues(t, 1, f.GetSpeed().GetAttributes().GetMinimumSpeed())
		assert.EqualValues(t, 5, f.GetSpeed().GetAttributes().GetMaximumSpeed())
		assert.EqualValues(t, 2, f.GetSpeed().GetState().GetCurrentSpeed())

		assert.Equal(t, []string{"Normal", "Natural", "Sleep"}, f.GetMode().GetAttributes().GetAvailableModes())
		assert.Equal(t, "Normal", f.GetMode().GetState().GetCurrentMode())

		assert.Equal(t, fanTimerDurations(), f.GetTimer().GetAttributes().GetAvailableDurations())
		assert.EqualValues(t, 0, f.GetTimer().GetState().GetRemainingSeconds())

		assert.ElementsMatch(t, []string{"oscillation", "beep"}, f.GetToggles().GetAttributes().GetAvailableToggles())
		assert.Equal(t, map[string]bool{"oscillation": true, "beep": false}, f.GetToggles().GetState().GetSettings())

		assert.Nil(t, f.Temperature, "temperature role wasn't configured")
	})

	t.Run("populates Temperature when the role is present", func(t *testing.T) {
		entities := fanEntities()
		entities["temperature"] = &esphome.SensorEntity{Key: 200007, State: 21.5}
		d, err := fanBuilder{}.build(deviceConfig{ID: "f1", Type: "fan"}, entities)
		require.NoError(t, err)

		require.NotNil(t, d.GetFan().GetTemperature())
		assert.Equal(t, "celsius", d.GetFan().GetTemperature().GetAttributes().GetUnit())
		assert.InDelta(t, 21.5, d.GetFan().GetTemperature().GetState().GetValue(), 0.001)
	})

	t.Run("timer state reflects the select entity's current label", func(t *testing.T) {
		entities := fanEntities()
		entities["timer"] = &esphome.SelectEntity{Key: fakeTimerKey, Options: fanTimerLabels, State: "3h"}
		d, err := fanBuilder{}.build(deviceConfig{ID: "f1", Type: "fan"}, entities)
		require.NoError(t, err)
		assert.EqualValues(t, 10800, d.GetFan().GetTimer().GetState().GetRemainingSeconds())
	})
}

func TestFanBuilder_ApplyState(t *testing.T) {
	newDevice := func() *device.Device {
		d, err := fanBuilder{}.build(deviceConfig{ID: "f1", Type: "fan"}, fanEntities())
		require.NoError(t, err)
		return d
	}

	t.Run("fan role updates OnOff and Speed", func(t *testing.T) {
		d := newDevice()
		fanBuilder{}.applyState(d, "fan", &pb.FanStateResponse{Key: fakeFanKey, State: true, SpeedLevel: 4})
		assert.True(t, d.GetFan().GetOnOff().GetState().GetIsOn())
		assert.EqualValues(t, 4, d.GetFan().GetSpeed().GetState().GetCurrentSpeed())
	})

	t.Run("mode role updates Mode", func(t *testing.T) {
		d := newDevice()
		fanBuilder{}.applyState(d, "mode", &pb.SelectStateResponse{Key: fakeModeKey, State: "Sleep"})
		assert.Equal(t, "Sleep", d.GetFan().GetMode().GetState().GetCurrentMode())
	})

	t.Run("timer role updates Timer via label parsing", func(t *testing.T) {
		d := newDevice()
		fanBuilder{}.applyState(d, "timer", &pb.SelectStateResponse{Key: fakeTimerKey, State: "2h"})
		assert.EqualValues(t, 7200, d.GetFan().GetTimer().GetState().GetRemainingSeconds())
	})

	t.Run("toggle roles update Toggles.State.Settings", func(t *testing.T) {
		d := newDevice()
		fanBuilder{}.applyState(d, "oscillation", &pb.SwitchStateResponse{Key: fakeOscKey, State: false})
		assert.False(t, d.GetFan().GetToggles().GetState().GetSettings()["oscillation"])
	})

	t.Run("temperature role ignored when trait wasn't built", func(t *testing.T) {
		d := newDevice()
		require.NotPanics(t, func() {
			fanBuilder{}.applyState(d, "temperature", &pb.SensorStateResponse{Key: 1, State: 20})
		})
	})

	t.Run("ignores a message of the wrong type", func(t *testing.T) {
		d := newDevice()
		fanBuilder{}.applyState(d, "fan", &pb.SwitchStateResponse{Key: fakeFanKey, State: true})
		assert.False(t, d.GetFan().GetOnOff().GetState().GetIsOn())
	})
}

func TestFanBuilder_ApplyCommand_ErrorsWithoutTouchingClient(t *testing.T) {
	newDeviceAndKeys := func() (*device.Device, map[string]uint32) {
		d, err := fanBuilder{}.build(deviceConfig{ID: "f1", Type: "fan"}, fanEntities())
		require.NoError(t, err)
		return d, map[string]uint32{
			"fan": fakeFanKey, "mode": fakeModeKey, "timer": fakeTimerKey,
			"oscillation": fakeOscKey, "beep": fakeBeepKey,
		}
	}

	t.Run("unsupported command type", func(t *testing.T) {
		d, roleKeys := newDeviceAndKeys()
		err := fanBuilder{}.applyCommand(nil, roleKeys, d, &command.Command{DeviceId: "f1"})
		assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
	})

	t.Run("mode value not in available_modes", func(t *testing.T) {
		d, roleKeys := newDeviceAndKeys()
		err := fanBuilder{}.applyCommand(nil, roleKeys, d, &command.Command{
			DeviceId: "f1",
			Details:  &command.Command_Mode{Mode: &command.Mode{Value: "Turbo"}},
		})
		assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
	})

	t.Run("timer duration not in the fixed set", func(t *testing.T) {
		d, roleKeys := newDeviceAndKeys()
		err := fanBuilder{}.applyCommand(nil, roleKeys, d, &command.Command{
			DeviceId: "f1",
			Details:  &command.Command_Timer{Timer: &command.Timer{DurationSeconds: 1800}},
		})
		assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
	})

	t.Run("unknown toggle name", func(t *testing.T) {
		d, roleKeys := newDeviceAndKeys()
		err := fanBuilder{}.applyCommand(nil, roleKeys, d, &command.Command{
			DeviceId: "f1",
			Details:  &command.Command_Toggle{Toggle: &command.Toggle{Settings: map[string]bool{"display": true}}},
		})
		assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
	})

	t.Run("missing role key for OnOff", func(t *testing.T) {
		d, _ := newDeviceAndKeys()
		err := fanBuilder{}.applyCommand(nil, map[string]uint32{}, d, &command.Command{
			DeviceId: "f1",
			Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
		})
		assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
	})
}

func TestFanBuilder_ApplyCommand_SendsToNode(t *testing.T) {
	server := newFakeESPHomeServer(t, nil)
	client := dialFakeServerClient(t, server)

	d, err := fanBuilder{}.build(deviceConfig{ID: "f1", Type: "fan"}, fanEntities())
	require.NoError(t, err)
	roleKeys := map[string]uint32{
		"fan": fakeFanKey, "mode": fakeModeKey, "timer": fakeTimerKey,
		"oscillation": fakeOscKey, "beep": fakeBeepKey,
	}

	t.Run("on/off and speed go to the fan entity", func(t *testing.T) {
		err := fanBuilder{}.applyCommand(client, roleKeys, d, &command.Command{
			DeviceId: "f1",
			Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
		})
		require.NoError(t, err)
		assert.True(t, d.GetFan().GetOnOff().GetState().GetIsOn())

		err = fanBuilder{}.applyCommand(client, roleKeys, d, &command.Command{
			DeviceId: "f1",
			Details:  &command.Command_Speed{Speed: &command.Speed{Value: 100}},
		})
		require.NoError(t, err)
		assert.EqualValues(t, 5, d.GetFan().GetSpeed().GetState().GetCurrentSpeed(), "clamped to maximum_speed")

		require.Eventually(t, func() bool { return len(server.FanCommands()) == 2 }, time.Second, 10*time.Millisecond)
		cmds := server.FanCommands()
		assert.True(t, cmds[0].HasState)
		assert.True(t, cmds[0].State)
		assert.True(t, cmds[1].HasSpeedLevel)
		assert.EqualValues(t, 5, cmds[1].SpeedLevel)
	})

	t.Run("mode goes to the select entity", func(t *testing.T) {
		err := fanBuilder{}.applyCommand(client, roleKeys, d, &command.Command{
			DeviceId: "f1",
			Details:  &command.Command_Mode{Mode: &command.Mode{Value: "Sleep"}},
		})
		require.NoError(t, err)
		assert.Equal(t, "Sleep", d.GetFan().GetMode().GetState().GetCurrentMode())

		require.Eventually(t, func() bool { return len(server.SelectCommands()) >= 1 }, time.Second, 10*time.Millisecond)
		assert.Equal(t, "Sleep", server.SelectCommands()[0].State)
	})

	t.Run("timer converts duration to its select label", func(t *testing.T) {
		err := fanBuilder{}.applyCommand(client, roleKeys, d, &command.Command{
			DeviceId: "f1",
			Details:  &command.Command_Timer{Timer: &command.Timer{DurationSeconds: 7200}},
		})
		require.NoError(t, err)
		assert.EqualValues(t, 7200, d.GetFan().GetTimer().GetState().GetRemainingSeconds())

		require.Eventually(t, func() bool { return len(server.SelectCommands()) >= 2 }, time.Second, 10*time.Millisecond)
		assert.Equal(t, "2h", server.SelectCommands()[len(server.SelectCommands())-1].State)
	})

	t.Run("toggle goes to the named switch entity", func(t *testing.T) {
		err := fanBuilder{}.applyCommand(client, roleKeys, d, &command.Command{
			DeviceId: "f1",
			Details:  &command.Command_Toggle{Toggle: &command.Toggle{Settings: map[string]bool{"oscillation": false}}},
		})
		require.NoError(t, err)
		assert.False(t, d.GetFan().GetToggles().GetState().GetSettings()["oscillation"])

		require.Eventually(t, func() bool { return len(server.SwitchCommands()) == 1 }, time.Second, 10*time.Millisecond)
		cmd := server.SwitchCommands()[0]
		assert.Equal(t, fakeOscKey, cmd.Key)
		assert.False(t, cmd.State)
	})
}

func TestDurationToTimerLabel(t *testing.T) {
	label, ok := durationToTimerLabel(0)
	require.True(t, ok)
	assert.Equal(t, "Cancel", label)

	label, ok = durationToTimerLabel(43200)
	require.True(t, ok)
	assert.Equal(t, "12h", label)

	_, ok = durationToTimerLabel(1800)
	assert.False(t, ok, "not in the fixed 1h-increment set")

	_, ok = durationToTimerLabel(46800)
	assert.False(t, ok, "beyond the fixed 12h maximum")
}

func TestTimerLabelToDuration(t *testing.T) {
	assert.EqualValues(t, 0, timerLabelToDuration("Cancel"))
	assert.EqualValues(t, 3600, timerLabelToDuration("1h"))
	assert.EqualValues(t, 43200, timerLabelToDuration("12h"))
	assert.EqualValues(t, 0, timerLabelToDuration("not-a-label"), "unrecognized label treated as not running")
}

func TestClampSpeed(t *testing.T) {
	assert.EqualValues(t, 1, clampSpeed(0, 1, 5))
	assert.EqualValues(t, 5, clampSpeed(9, 1, 5))
	assert.EqualValues(t, 3, clampSpeed(3, 1, 5))
}

// --- standingDeskBuilder ---

const (
	fakeHeightKey   uint32 = 300001
	fakeMoveUpKey   uint32 = 300002
	fakeMoveDownKey uint32 = 300003
	fakePresetKey   uint32 = 300004
)

func standingDeskEntities() map[string]esphome.Entity {
	return map[string]esphome.Entity{
		"height":         &esphome.SensorEntity{Key: fakeHeightKey, State: 90},
		"move_up":        &esphome.SwitchEntity{Key: fakeMoveUpKey},
		"move_down":      &esphome.SwitchEntity{Key: fakeMoveDownKey},
		"preset_3_stand": &esphome.ButtonEntity{Key: fakePresetKey},
	}
}

func standingDeskConfig() deviceConfig {
	min, max := float32(65), float32(130)
	return deviceConfig{ID: "d1", Type: "standing_desk", PositionMin: &min, PositionMax: &max}
}

func TestStandingDeskBuilder_Build(t *testing.T) {
	t.Run("missing height role errors", func(t *testing.T) {
		entities := standingDeskEntities()
		delete(entities, "height")
		_, err := standingDeskBuilder{}.build(standingDeskConfig(), entities)
		assert.Error(t, err)
	})

	t.Run("missing move_up role errors", func(t *testing.T) {
		entities := standingDeskEntities()
		delete(entities, "move_up")
		_, err := standingDeskBuilder{}.build(standingDeskConfig(), entities)
		assert.Error(t, err)
	})

	t.Run("missing move_down role errors", func(t *testing.T) {
		entities := standingDeskEntities()
		delete(entities, "move_down")
		_, err := standingDeskBuilder{}.build(standingDeskConfig(), entities)
		assert.Error(t, err)
	})

	t.Run("missing position_min/position_max errors", func(t *testing.T) {
		_, err := standingDeskBuilder{}.build(deviceConfig{ID: "d1", Type: "standing_desk"}, standingDeskEntities())
		assert.Error(t, err)
	})

	t.Run("a preset role that isn't a button entity errors", func(t *testing.T) {
		entities := standingDeskEntities()
		entities["preset_3_stand"] = &esphome.SwitchEntity{Key: fakePresetKey}
		_, err := standingDeskBuilder{}.build(standingDeskConfig(), entities)
		assert.Error(t, err)
	})

	t.Run("populates Position and Mode, leaves Movement unset", func(t *testing.T) {
		d, err := standingDeskBuilder{}.build(standingDeskConfig(), standingDeskEntities())
		require.NoError(t, err)

		sd := d.GetStandingDesk()
		require.NotNil(t, sd)
		assert.Equal(t, "ESPHome", d.Manufacturer)

		assert.Equal(t, "cm", sd.GetPosition().GetAttributes().GetUnit())
		assert.EqualValues(t, 65, sd.GetPosition().GetAttributes().GetMinValue())
		assert.EqualValues(t, 130, sd.GetPosition().GetAttributes().GetMaxValue())
		assert.False(t, sd.GetPosition().GetAttributes().GetSupportsSet())
		assert.EqualValues(t, 90, sd.GetPosition().GetState().GetValue())
		assert.InDelta(t, 38.46, sd.GetPosition().GetState().GetPercent(), 0.01)

		assert.True(t, sd.GetMode().GetAttributes().GetCanControl())
		assert.Equal(t, []string{"preset_3_stand"}, sd.GetMode().GetAttributes().GetAvailableModes())

		assert.Nil(t, sd.Movement)
	})
}

func TestStandingDeskBuilder_ApplyState(t *testing.T) {
	newDevice := func() *device.Device {
		d, err := standingDeskBuilder{}.build(standingDeskConfig(), standingDeskEntities())
		require.NoError(t, err)
		return d
	}

	t.Run("height role updates Position", func(t *testing.T) {
		d := newDevice()
		standingDeskBuilder{}.applyState(d, "height", &pb.SensorStateResponse{Key: fakeHeightKey, State: 110})
		assert.EqualValues(t, 110, d.GetStandingDesk().GetPosition().GetState().GetValue())
		assert.InDelta(t, 69.23, d.GetStandingDesk().GetPosition().GetState().GetPercent(), 0.01)
	})

	t.Run("ignores a role that isn't height", func(t *testing.T) {
		d := newDevice()
		standingDeskBuilder{}.applyState(d, "move_up", &pb.SensorStateResponse{Key: fakeHeightKey, State: 110})
		assert.EqualValues(t, 90, d.GetStandingDesk().GetPosition().GetState().GetValue())
	})

	t.Run("ignores a message of the wrong type", func(t *testing.T) {
		d := newDevice()
		standingDeskBuilder{}.applyState(d, "height", &pb.SwitchStateResponse{Key: fakeHeightKey, State: true})
		assert.EqualValues(t, 90, d.GetStandingDesk().GetPosition().GetState().GetValue())
	})
}

func TestStandingDeskBuilder_ApplyCommand_ErrorsWithoutTouchingClient(t *testing.T) {
	newDeviceAndKeys := func() (*device.Device, map[string]uint32) {
		d, err := standingDeskBuilder{}.build(standingDeskConfig(), standingDeskEntities())
		require.NoError(t, err)
		return d, map[string]uint32{
			"height": fakeHeightKey, "move_up": fakeMoveUpKey, "move_down": fakeMoveDownKey,
			"preset_3_stand": fakePresetKey,
		}
	}

	t.Run("unknown preset value", func(t *testing.T) {
		d, roleKeys := newDeviceAndKeys()
		err := standingDeskBuilder{}.applyCommand(nil, roleKeys, d, &command.Command{
			DeviceId: "d1",
			Details:  &command.Command_Mode{Mode: &command.Mode{Value: "preset_1"}},
		})
		assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
	})

	t.Run("Position command is always unsupported", func(t *testing.T) {
		d, roleKeys := newDeviceAndKeys()
		err := standingDeskBuilder{}.applyCommand(nil, roleKeys, d, &command.Command{
			DeviceId: "d1",
			Details:  &command.Command_Position{Position: &command.Position{Target: &command.Position_Value{Value: 100}}},
		})
		assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
	})

	t.Run("Movement command is unsupported until the crash bug is resolved", func(t *testing.T) {
		d, roleKeys := newDeviceAndKeys()
		err := standingDeskBuilder{}.applyCommand(nil, roleKeys, d, &command.Command{
			DeviceId: "d1",
			Details:  &command.Command_Movement{Movement: &command.Movement{Direction: trait.Movement_DIRECTION_POSITIVE}},
		})
		assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
	})

	t.Run("unsupported command type", func(t *testing.T) {
		d, roleKeys := newDeviceAndKeys()
		err := standingDeskBuilder{}.applyCommand(nil, roleKeys, d, &command.Command{DeviceId: "d1"})
		assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
	})
}

func TestStandingDeskBuilder_ApplyCommand_SendsToNode(t *testing.T) {
	server := newFakeESPHomeServer(t, nil)
	client := dialFakeServerClient(t, server)

	d, err := standingDeskBuilder{}.build(standingDeskConfig(), standingDeskEntities())
	require.NoError(t, err)
	roleKeys := map[string]uint32{
		"height": fakeHeightKey, "move_up": fakeMoveUpKey, "move_down": fakeMoveDownKey,
		"preset_3_stand": fakePresetKey,
	}

	err = standingDeskBuilder{}.applyCommand(client, roleKeys, d, &command.Command{
		DeviceId: "d1",
		Details:  &command.Command_Mode{Mode: &command.Mode{Value: "preset_3_stand"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "preset_3_stand", d.GetStandingDesk().GetMode().GetState().GetCurrentMode())

	require.Eventually(t, func() bool { return len(server.ButtonCommands()) == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, fakePresetKey, server.ButtonCommands()[0].Key)
}

func TestPositionPercent(t *testing.T) {
	assert.InDelta(t, 0, positionPercent(65, 65, 130), 0.001)
	assert.InDelta(t, 100, positionPercent(130, 65, 130), 0.001)
	assert.InDelta(t, 50, positionPercent(97.5, 65, 130), 0.001)
	assert.EqualValues(t, 0, positionPercent(50, 65, 65), "degenerate range doesn't divide by zero")
}
