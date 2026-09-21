package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nanoleaf "github.com/rmrobinson/nanoleaf-go"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/service/bridge"
)

// fakePanelClient is an in-memory panelClient, recording every call made against it. errToReturn,
// if set, is returned by every method instead of succeeding.
type fakePanelClient struct {
	panel   *nanoleaf.LightPanel
	effects []nanoleaf.Effect

	errToReturn error
	// errOnSetSaturation, if set, fails only SetSaturation (independent of errToReturn) - used to
	// exercise a partial failure partway through a multi-call command.
	errOnSetSaturation error

	onCalls         []bool
	brightnessCalls []int
	hueCalls        []int
	satCalls        []int
	ctCalls         []int
	sceneCalls      []string
}

func (f *fakePanelClient) GetPanel(ctx context.Context) (*nanoleaf.LightPanel, error) {
	if f.errToReturn != nil {
		return nil, f.errToReturn
	}
	return f.panel, nil
}

func (f *fakePanelClient) GetEffects(ctx context.Context) ([]nanoleaf.Effect, error) {
	if f.errToReturn != nil {
		return nil, f.errToReturn
	}
	return f.effects, nil
}

func (f *fakePanelClient) SetOn(ctx context.Context, on bool) error {
	if f.errToReturn != nil {
		return f.errToReturn
	}
	f.onCalls = append(f.onCalls, on)
	return nil
}

func (f *fakePanelClient) SetBrightness(ctx context.Context, level, duration int) error {
	if f.errToReturn != nil {
		return f.errToReturn
	}
	f.brightnessCalls = append(f.brightnessCalls, level)
	return nil
}

func (f *fakePanelClient) SetHue(ctx context.Context, hue int) error {
	if f.errToReturn != nil {
		return f.errToReturn
	}
	f.hueCalls = append(f.hueCalls, hue)
	return nil
}

func (f *fakePanelClient) SetSaturation(ctx context.Context, sat int) error {
	if f.errToReturn != nil {
		return f.errToReturn
	}
	if f.errOnSetSaturation != nil {
		return f.errOnSetSaturation
	}
	f.satCalls = append(f.satCalls, sat)
	return nil
}

func (f *fakePanelClient) SetCT(ctx context.Context, ct int) error {
	if f.errToReturn != nil {
		return f.errToReturn
	}
	f.ctCalls = append(f.ctCalls, ct)
	return nil
}

func (f *fakePanelClient) SetScene(ctx context.Context, name string) error {
	if f.errToReturn != nil {
		return f.errToReturn
	}
	f.sceneCalls = append(f.sceneCalls, name)
	return nil
}

func (f *fakePanelClient) SubscribeEvents(ctx context.Context, ids ...int) (<-chan *nanoleaf.PanelUpdate, <-chan error) {
	updates := make(chan *nanoleaf.PanelUpdate)
	errs := make(chan error)
	return updates, errs
}

func testPanel() *nanoleaf.LightPanel {
	return &nanoleaf.LightPanel{
		Name:         "Den Panels",
		SerialNumber: "S1",
		Manufacturer: "Nanoleaf",
		ModelNumber:  "NL29",
		State: nanoleaf.PanelState{
			On:         &nanoleaf.BoolValue{Value: true},
			Brightness: &nanoleaf.IntRangeValue{Value: 80, Min: 0, Max: 100},
			Hue:        &nanoleaf.IntRangeValue{Value: 120, Min: 0, Max: 360},
			Saturation: &nanoleaf.IntRangeValue{Value: 50, Min: 0, Max: 100},
			CT:         &nanoleaf.IntRangeValue{Value: 4000, Min: 1200, Max: 6500},
		},
		Effect: nanoleaf.PanelEffect{
			Current: "Northern Lights",
			Options: []string{"Northern Lights", "Nemo"},
		},
	}
}

func testEffects() []nanoleaf.Effect {
	return []nanoleaf.Effect{
		{Name: "Northern Lights"},
		{Name: "Nemo"},
	}
}

func TestBuildDevice(t *testing.T) {
	cfg := deviceConfig{ID: "nanoleaf-den", Host: "10.0.0.5"}
	d := buildDevice(cfg, testPanel(), testEffects())

	assert.Equal(t, "nanoleaf-den", d.Id)
	assert.Equal(t, "NL29", d.ModelId)
	assert.Equal(t, "Nanoleaf", d.Manufacturer)
	// Falls back to the panel's own reported name since cfg.Name is unset.
	assert.Equal(t, "Den Panels", d.Config.Name)
	assert.Equal(t, "10.0.0.5", d.Address.Address)
	assert.True(t, d.Address.IsReachable)

	l := d.GetLight()
	require.NotNil(t, l)
	assert.True(t, l.OnOff.State.IsOn)
	assert.EqualValues(t, 80, l.Brightness.State.Level)
	assert.EqualValues(t, 120, l.Colour.State.Hsb.Hue)
	assert.EqualValues(t, 50, l.Colour.State.Hsb.Saturation)
	assert.EqualValues(t, 4000, l.Colour.State.ColourTemperatureK)
	require.NotNil(t, l.Colour.Attributes.ColourTemperatureRange)
	assert.EqualValues(t, 1200, l.Colour.Attributes.ColourTemperatureRange.MinK)
	assert.EqualValues(t, 6500, l.Colour.Attributes.ColourTemperatureRange.MaxK)

	require.NotNil(t, l.Scene)
	assert.Equal(t, "Northern Lights", l.Scene.State.ApplicationId)
	require.Len(t, l.Scene.Attributes.Applications, 2)
	assert.Equal(t, "Northern Lights", l.Scene.Attributes.Applications[0].Id)
}

func TestBuildDevice_ConfiguredNameWins(t *testing.T) {
	cfg := deviceConfig{ID: "nanoleaf-den", Name: "Custom Name", Host: "10.0.0.5"}
	d := buildDevice(cfg, testPanel(), testEffects())
	assert.Equal(t, "Custom Name", d.Config.Name)
}

// TestBuildDevice_NilCT covers a panel reporting its "ct" state as JSON null (e.g. a model with
// no colour-temperature support) - buildDevice must not dereference panel.State.CT in that case.
func TestBuildDevice_NilCT(t *testing.T) {
	cfg := deviceConfig{ID: "nanoleaf-den", Host: "10.0.0.5"}
	panel := testPanel()
	panel.State.CT = nil

	require.NotPanics(t, func() {
		d := buildDevice(cfg, panel, testEffects())
		l := d.GetLight()
		assert.Nil(t, l.Colour.Attributes.ColourTemperatureRange)
		assert.EqualValues(t, 0, l.Colour.State.ColourTemperatureK)
		// Still true: CT alone is enough to control Colour even without hue/saturation.
		assert.True(t, l.Colour.Attributes.CanControl)
	})
}

// TestBuildDevice_NilOnBrightnessHueSaturation covers every other PanelState field that, like CT,
// can independently be JSON null - a panel that omits all colour capability (e.g. a white-only
// model) must not panic buildDevice, and must stop claiming it can control Colour.
func TestBuildDevice_NilOnBrightnessHueSaturation(t *testing.T) {
	cfg := deviceConfig{ID: "nanoleaf-den", Host: "10.0.0.5"}
	panel := testPanel()
	panel.State.On = nil
	panel.State.Brightness = nil
	panel.State.Hue = nil
	panel.State.Saturation = nil
	panel.State.CT = nil

	require.NotPanics(t, func() {
		d := buildDevice(cfg, panel, testEffects())
		l := d.GetLight()
		assert.False(t, l.OnOff.State.IsOn)
		assert.EqualValues(t, 0, l.Brightness.State.Level)
		assert.EqualValues(t, 0, l.Colour.State.Hsb.Hue)
		assert.EqualValues(t, 0, l.Colour.State.Hsb.Saturation)
		assert.False(t, l.Colour.Attributes.CanControl)
	})
}

// TestBuildDevice_NilHueSaturationOnly covers a panel that reports CT but not hue/saturation -
// Colour should still be controllable (via CT), just not via HSB.
func TestBuildDevice_NilHueSaturationOnly(t *testing.T) {
	cfg := deviceConfig{ID: "nanoleaf-den", Host: "10.0.0.5"}
	panel := testPanel()
	panel.State.Hue = nil
	panel.State.Saturation = nil

	d := buildDevice(cfg, panel, testEffects())
	assert.True(t, d.GetLight().Colour.Attributes.CanControl)
}

func newTestPanelConn(t *testing.T, fc *fakePanelClient) *panelConn {
	t.Helper()
	pc := &panelConn{
		logger: zaptest.NewLogger(t),
		svc:    bridge.NewService(zaptest.NewLogger(t)),
		cfg:    deviceConfig{ID: "nanoleaf-den", Host: "10.0.0.5"},
		client: fc,
	}
	pc.device = buildDevice(pc.cfg, fc.panel, fc.effects)
	return pc
}

func TestApplyCommand_OnOff(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel()}
	pc := newTestPanelConn(t, fc)

	d, err := pc.applyCommand(context.Background(), &command.Command{
		DeviceId: "nanoleaf-den",
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: false}},
	})
	require.NoError(t, err)
	assert.Equal(t, []bool{false}, fc.onCalls)
	assert.False(t, d.GetLight().OnOff.State.IsOn)
}

func TestApplyCommand_BrightnessAbsolute(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel()}
	pc := newTestPanelConn(t, fc)

	d, err := pc.applyCommand(context.Background(), &command.Command{
		Details: &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: 150}},
	})
	require.NoError(t, err)
	// Clamped to 100.
	assert.Equal(t, []int{100}, fc.brightnessCalls)
	assert.EqualValues(t, 100, d.GetLight().Brightness.State.Level)
}

func TestApplyCommand_BrightnessRelative(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel()} // starts at 80
	pc := newTestPanelConn(t, fc)

	d, err := pc.applyCommand(context.Background(), &command.Command{
		Details: &command.Command_BrightnessRelative{BrightnessRelative: &command.BrightnessRelative{ChangePercent: 30}},
	})
	require.NoError(t, err)
	// 80 + 30 = 110, clamped to 100.
	assert.Equal(t, []int{100}, fc.brightnessCalls)
	assert.EqualValues(t, 100, d.GetLight().Brightness.State.Level)
}

func TestApplyCommand_ColourHSB(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel()}
	pc := newTestPanelConn(t, fc)

	d, err := pc.applyCommand(context.Background(), &command.Command{
		Details: &command.Command_Colour{Colour: &command.Colour{
			Value: &command.Colour_Hsb{Hsb: &command.Colour_HSB{Hue: 200, Saturation: 75}},
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{200}, fc.hueCalls)
	assert.Equal(t, []int{75}, fc.satCalls)
	assert.EqualValues(t, 200, d.GetLight().Colour.State.Hsb.Hue)
	assert.EqualValues(t, 75, d.GetLight().Colour.State.Hsb.Saturation)
}

// TestApplyCommand_ColourHSB_PartialFailure covers SetHue succeeding but SetSaturation failing -
// the cache must reflect the hue change that actually reached the panel, not silently keep the
// old value as if nothing had happened.
func TestApplyCommand_ColourHSB_PartialFailure(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel(), errOnSetSaturation: errors.New("boom")}
	pc := newTestPanelConn(t, fc)

	_, err := pc.applyCommand(context.Background(), &command.Command{
		Details: &command.Command_Colour{Colour: &command.Colour{
			Value: &command.Colour_Hsb{Hsb: &command.Colour_HSB{Hue: 200, Saturation: 75}},
		}},
	})
	require.Error(t, err)
	assert.Equal(t, []int{200}, fc.hueCalls)
	assert.Empty(t, fc.satCalls)
	// Hue's change reached the panel and is reflected; saturation's didn't, so it's untouched.
	assert.EqualValues(t, 200, pc.device.GetLight().Colour.State.Hsb.Hue)
	assert.EqualValues(t, 50, pc.device.GetLight().Colour.State.Hsb.Saturation)
}

func TestApplyCommand_ColourTemperature_ClampedToRange(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel()} // range is 1200-6500
	pc := newTestPanelConn(t, fc)

	d, err := pc.applyCommand(context.Background(), &command.Command{
		Details: &command.Command_Colour{Colour: &command.Colour{
			Value: &command.Colour_ColourTemperatureK{ColourTemperatureK: 9000},
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{6500}, fc.ctCalls)
	assert.EqualValues(t, 6500, d.GetLight().Colour.State.ColourTemperatureK)
}

func TestApplyCommand_ColourRGB_Unsupported(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel()}
	pc := newTestPanelConn(t, fc)

	_, err := pc.applyCommand(context.Background(), &command.Command{
		Details: &command.Command_Colour{Colour: &command.Colour{
			Value: &command.Colour_Rgb{Rgb: &command.Colour_RGB{Red: 255}},
		}},
	})
	assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
	assert.Empty(t, fc.hueCalls)
}

func TestApplyCommand_AppLaunch(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel()}
	pc := newTestPanelConn(t, fc)

	d, err := pc.applyCommand(context.Background(), &command.Command{
		Details: &command.Command_AppLaunch{AppLaunch: &command.AppLaunch{ApplicationId: "Nemo"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"Nemo"}, fc.sceneCalls)
	assert.Equal(t, "Nemo", d.GetLight().Scene.State.ApplicationId)
}

func TestApplyCommand_UnsupportedCommand(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel()}
	pc := newTestPanelConn(t, fc)

	_, err := pc.applyCommand(context.Background(), &command.Command{
		Details: &command.Command_Toggle{Toggle: &command.Toggle{}},
	})
	assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
}

func TestApplyCommand_NoDevice(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel()}
	pc := newTestPanelConn(t, fc)
	pc.device = nil

	_, err := pc.applyCommand(context.Background(), &command.Command{
		Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	assert.ErrorIs(t, err, bridge.ErrDeviceNotFound)
}

func TestApplyCommand_ClientErrorWrapsToInternal(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel(), errToReturn: errors.New("boom")}
	pc := newTestPanelConn(t, fc)

	_, err := pc.applyCommand(context.Background(), &command.Command{
		Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Internal, st.Code())
}

func TestApplyUpdate_State(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel()}
	pc := newTestPanelConn(t, fc)

	pc.applyUpdate(&nanoleaf.PanelUpdate{
		TypeID: stateEventID,
		State: &nanoleaf.PanelState{
			On:         &nanoleaf.BoolValue{Value: false},
			Brightness: &nanoleaf.IntRangeValue{Value: 10},
		},
	})

	l := pc.device.GetLight()
	assert.False(t, l.OnOff.State.IsOn)
	assert.EqualValues(t, 10, l.Brightness.State.Level)
	// Hue wasn't part of this update - stays at its seeded value.
	assert.EqualValues(t, 120, l.Colour.State.Hsb.Hue)
	assert.True(t, pc.device.Address.IsReachable)
}

func TestApplyUpdate_Effects(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel()}
	pc := newTestPanelConn(t, fc)

	pc.applyUpdate(&nanoleaf.PanelUpdate{
		TypeID: effectsEventID,
		Effect: &nanoleaf.PanelEffect{Current: "Nemo"},
	})

	assert.Equal(t, "Nemo", pc.device.GetLight().Scene.State.ApplicationId)
}

func TestApplyUpdate_UnknownTypeIgnored(t *testing.T) {
	fc := &fakePanelClient{panel: testPanel()}
	pc := newTestPanelConn(t, fc)

	before := pc.device.GetLight().OnOff.State.IsOn
	pc.applyUpdate(&nanoleaf.PanelUpdate{TypeID: 4})
	assert.Equal(t, before, pc.device.GetLight().OnOff.State.IsOn)
}

func TestClampCT_NoRangeReturnsUnclamped(t *testing.T) {
	assert.EqualValues(t, 9000, clampCT(9000, nil))
}
