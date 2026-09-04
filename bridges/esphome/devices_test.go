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
