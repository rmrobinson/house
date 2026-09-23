package main

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	nanoleaf "github.com/rmrobinson/nanoleaf-go"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

// initialSeedRetryDelay is how long panelConn.run waits between attempts to fetch a panel's
// initial state when it's unreachable at startup (or a reconnect). Fixed, not exponential -
// mirrors bridges/esphome's dial retry loop; SubscribeEvents' own backoff (nanoleaf-go's
// stream.go) is what handles backoff for the live event stream once seeding has succeeded once.
const initialSeedRetryDelay = 5 * time.Second

// panelRequestTimeout bounds every request-response call to a panel (GetPanel/GetEffects during
// seeding, and every Set* command) - a panel that accepts a TCP connection but never answers
// would otherwise hang the caller indefinitely, and for commands that means holding panelConn.mu
// for as long as the hang lasts, freezing that panel's SSE-driven state updates too. Deliberately
// NOT set as the shared *http.Client's Timeout: that client is also used for the long-lived
// SubscribeEvents stream, where a whole-request timeout would cut the connection every
// panelRequestTimeout seconds instead of leaving it open.
const panelRequestTimeout = 10 * time.Second

// stateEventID and effectsEventID are the Nanoleaf /events type IDs this bridge subscribes to -
// see nanoleaf-go's event.go. layoutEventID (2) and touchEventID (4) are deliberately not
// subscribed to: this bridge has no trait that models panel geometry or touch gestures.
const (
	stateEventID   = 1
	effectsEventID = 3
)

// panelClient is the subset of *nanoleaf.Client a panelConn needs. Defined as an interface so
// applyCommand/applyUpdate/buildDevice can be tested against a fake without a live HTTP/SSE
// connection to a panel - same technique as bridges/cast's castSession.
type panelClient interface {
	GetPanel(ctx context.Context) (*nanoleaf.LightPanel, error)
	GetEffects(ctx context.Context) ([]nanoleaf.Effect, error)
	SetOn(ctx context.Context, on bool) error
	SetBrightness(ctx context.Context, level, duration int) error
	SetHue(ctx context.Context, hue int) error
	SetSaturation(ctx context.Context, sat int) error
	SetCT(ctx context.Context, ct int) error
	SetScene(ctx context.Context, name string) error
	SubscribeEvents(ctx context.Context, ids ...int) (<-chan *nanoleaf.PanelUpdate, <-chan error)
}

// panelConn owns a single Nanoleaf panel's connection and the one house device built from it.
type panelConn struct {
	logger *zap.Logger
	svc    *bridge.Service
	nb     *NanoleafBridge
	cfg    deviceConfig

	client panelClient

	mu     sync.Mutex
	device *device.Device
}

func newPanelConn(logger *zap.Logger, svc *bridge.Service, nb *NanoleafBridge, cfg deviceConfig) *panelConn {
	return &panelConn{
		logger: logger.With(zap.String("device_id", cfg.ID)),
		svc:    svc,
		nb:     nb,
		cfg:    cfg,
		client: nanoleaf.NewClient(&http.Client{}, cfg.Host, cfg.Port, cfg.APIKey),
	}
}

// run seeds the initial device from the panel's current state (retrying on a fixed delay until
// the panel answers) and then applies every update from its event stream until ctx is canceled.
func (pc *panelConn) run(ctx context.Context) {
	var panel *nanoleaf.LightPanel
	for {
		seedCtx, cancel := context.WithTimeout(ctx, panelRequestTimeout)
		p, err := pc.client.GetPanel(seedCtx)
		cancel()
		if err == nil {
			panel = p
			break
		}
		if ctx.Err() != nil {
			return
		}
		pc.logger.Error("unable to fetch initial panel state, retrying", zap.Error(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(initialSeedRetryDelay):
		}
	}

	effectsCtx, cancel := context.WithTimeout(ctx, panelRequestTimeout)
	effects, err := pc.client.GetEffects(effectsCtx)
	cancel()
	if err != nil {
		// Non-fatal: the device still builds, just with an empty effect catalog. A later
		// effects(3) event can still update State.ApplicationId even though
		// Attributes.Applications stays empty until the next bridge restart.
		pc.logger.Error("unable to fetch effects list", zap.Error(err))
	}

	pc.mu.Lock()
	pc.device = buildDevice(pc.cfg, panel, effects)
	d := proto.Clone(pc.device).(*device.Device)
	pc.mu.Unlock()

	pc.nb.registerDevice(pc.cfg.ID, pc)
	pc.svc.UpdateDevice(d)

	updates, errs := pc.client.SubscribeEvents(ctx, stateEventID, effectsEventID)
	for {
		select {
		case <-ctx.Done():
			return
		case u, ok := <-updates:
			if !ok {
				return
			}
			pc.applyUpdate(u)
		case err, ok := <-errs:
			if !ok {
				continue
			}
			pc.logger.Warn("panel event stream error, will keep retrying", zap.Error(err))
		}
	}
}

// applyUpdate merges one PanelUpdate onto the cached device and publishes the result.
func (pc *panelConn) applyUpdate(u *nanoleaf.PanelUpdate) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.device == nil {
		return
	}
	l := pc.device.GetLight()

	switch u.TypeID {
	case stateEventID:
		if u.State == nil {
			return
		}
		s := u.State
		if s.On != nil {
			l.OnOff.State.IsOn = s.On.Value
		}
		if s.Brightness != nil {
			l.Brightness.State.Level = int32(s.Brightness.Value)
		}
		if s.Hue != nil {
			l.Colour.State.Hsb.Hue = int32(s.Hue.Value)
		}
		if s.Saturation != nil {
			l.Colour.State.Hsb.Saturation = int32(s.Saturation.Value)
		}
		if s.CT != nil {
			l.Colour.State.ColourTemperatureK = int32(s.CT.Value)
		}
	case effectsEventID:
		if u.Effect == nil {
			return
		}
		l.Scene.State.ApplicationId = u.Effect.Current
	default:
		return
	}

	if pc.device.Address == nil {
		pc.device.Address = &device.Device_Address{}
	}
	pc.device.Address.IsReachable = true

	pc.svc.UpdateDevice(proto.Clone(pc.device).(*device.Device))
}

// applyCommand translates a house Command into the matching nanoleaf.Client call, updating the
// cached device optimistically. The event stream corrects this if the panel disagrees.
//
// ctx is bounded to panelRequestTimeout regardless of the caller's own deadline: this holds
// panelConn.mu for the whole call (applyUpdate needs it too, to apply live SSE state), so an
// unresponsive panel must not be allowed to block it indefinitely.
func (pc *panelConn) applyCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	ctx, cancel := context.WithTimeout(ctx, panelRequestTimeout)
	defer cancel()

	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.device == nil {
		return nil, bridge.ErrDeviceNotFound
	}
	l := pc.device.GetLight()

	var err error
	switch {
	case cmd.GetOnOff() != nil:
		on := cmd.GetOnOff().On
		if err = pc.client.SetOn(ctx, on); err == nil {
			l.OnOff.State.IsOn = on
		}

	case cmd.GetBrightnessAbsolute() != nil:
		pct := clampPercent(cmd.GetBrightnessAbsolute().BrightnessPercent)
		if err = pc.client.SetBrightness(ctx, int(pct), 0); err == nil {
			l.Brightness.State.Level = pct
		}

	case cmd.GetBrightnessRelative() != nil:
		pct := clampPercent(l.Brightness.State.Level + cmd.GetBrightnessRelative().ChangePercent)
		if err = pc.client.SetBrightness(ctx, int(pct), 0); err == nil {
			l.Brightness.State.Level = pct
		}

	case cmd.GetColour() != nil:
		err = pc.applyColourCommand(ctx, l, cmd.GetColour())

	case cmd.GetAppLaunch() != nil:
		name := cmd.GetAppLaunch().ApplicationId
		if err = pc.client.SetScene(ctx, name); err == nil {
			l.Scene.State.ApplicationId = name
		}

	default:
		return nil, bridge.ErrUnsupportedCommand
	}

	if err != nil {
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		pc.logger.Error("unable to send command to panel", zap.String("device_id", cmd.DeviceId), zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to send command to panel")
	}
	return proto.Clone(pc.device).(*device.Device), nil
}

// applyColourCommand handles the Colour oneof. Nanoleaf has no RGB endpoint - only hue/saturation
// and colour temperature - so a Colour.Rgb command is rejected as unsupported rather than
// silently dropped or approximated.
func (pc *panelConn) applyColourCommand(ctx context.Context, l *device.Light, c *command.Colour) error {
	switch v := c.GetValue().(type) {
	case *command.Colour_Hsb:
		hue := clampHue(v.Hsb.Hue)
		sat := clampPercent(v.Hsb.Saturation)
		// Hue and saturation are two separate panel requests - update the cache after each one
		// that actually succeeds, rather than only on full success, so a failure partway through
		// (the panel's hue changed but its saturation didn't) doesn't leave the cache reporting
		// neither took effect.
		if err := pc.client.SetHue(ctx, int(hue)); err != nil {
			return err
		}
		l.Colour.State.Hsb.Hue = hue
		if err := pc.client.SetSaturation(ctx, int(sat)); err != nil {
			return err
		}
		l.Colour.State.Hsb.Saturation = sat
		return nil

	case *command.Colour_ColourTemperatureK:
		ct := clampCT(v.ColourTemperatureK, l.Colour.Attributes.ColourTemperatureRange)
		if err := pc.client.SetCT(ctx, int(ct)); err != nil {
			return err
		}
		l.Colour.State.ColourTemperatureK = ct
		return nil

	default:
		return bridge.ErrUnsupportedCommand
	}
}

// onValue, intValue, and ctRange safely read a nanoleaf.PanelState field that can be nil - not
// just absent from a JSON response (GetPanel pre-allocates every one of these as a zero-value
// fallback for that case), but explicitly JSON null, which some panel models report for a
// capability they don't have (e.g. hue/saturation on a white-only panel, or ct on a non-tunable
// one). buildDevice must not assume any of them is present.
func onValue(v *nanoleaf.BoolValue) bool {
	if v == nil {
		return false
	}
	return v.Value
}

func intValue(v *nanoleaf.IntRangeValue) int32 {
	if v == nil {
		return 0
	}
	return int32(v.Value)
}

func ctRange(v *nanoleaf.IntRangeValue) *trait.Colour_Attributes_ColourTemperatureRange {
	if v == nil {
		return nil
	}
	return &trait.Colour_Attributes_ColourTemperatureRange{MinK: int32(v.Min), MaxK: int32(v.Max)}
}

// buildDevice constructs the initial house Light device from a panel's current state and effect
// catalog.
func buildDevice(cfg deviceConfig, panel *nanoleaf.LightPanel, effects []nanoleaf.Effect) *device.Device {
	apps := make([]*trait.App_Instance, 0, len(effects))
	for _, e := range effects {
		apps = append(apps, &trait.App_Instance{Id: e.Name, Name: e.Name})
	}

	// CanControl reflects what this specific panel actually reported supporting: a device with
	// neither hue/saturation nor colour temperature can't usefully take a Colour command, so it
	// shouldn't claim it can. Mode stays MODE_HSB regardless - Nanoleaf's wire format for a
	// non-CT colour representation is always hue/saturation, never RGB.
	canControlColour := (panel.State.Hue != nil && panel.State.Saturation != nil) || panel.State.CT != nil
	colourAttrs := &trait.Colour_Attributes{
		CanControl:             canControlColour,
		Mode:                   trait.Colour_Attributes_MODE_HSB,
		ColourTemperatureRange: ctRange(panel.State.CT),
	}

	name := cfg.Name
	if name == "" {
		name = panel.Name
	}

	return &device.Device{
		Id:           cfg.ID,
		ModelId:      panel.ModelNumber,
		Manufacturer: "Nanoleaf",
		Config: &device.Device_Config{
			Name: name,
		},
		Address: &device.Device_Address{
			Address:     cfg.Host,
			IsReachable: true,
		},
		Details: &device.Device_Light{
			Light: &device.Light{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{CanControl: true},
					State:      &trait.OnOff_State{IsOn: onValue(panel.State.On)},
				},
				Brightness: &trait.Brightness{
					Attributes: &trait.Brightness_Attributes{CanControl: true},
					State:      &trait.Brightness_State{Level: intValue(panel.State.Brightness)},
				},
				Colour: &trait.Colour{
					Attributes: colourAttrs,
					State: &trait.Colour_State{
						Hsb: &trait.Colour_State_HSB{
							Hue:        intValue(panel.State.Hue),
							Saturation: intValue(panel.State.Saturation),
						},
						ColourTemperatureK: intValue(panel.State.CT),
					},
				},
				Scene: &trait.App{
					Attributes: &trait.App_Attributes{CanControl: true, Applications: apps},
					State:      &trait.App_State{ApplicationId: panel.Effect.Current},
				},
			},
		},
	}
}

func clampPercent(pct int32) int32 {
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// clampHue clamps a command hue to the Colour trait's documented 0-360 degree range.
func clampHue(hue int32) int32 {
	if hue < 0 {
		return 0
	}
	if hue > 360 {
		return 360
	}
	return hue
}

// clampCT clamps a command colour temperature to the device's advertised range, if known.
func clampCT(ct int32, r *trait.Colour_Attributes_ColourTemperatureRange) int32 {
	if r == nil {
		return ct
	}
	if ct < r.MinK {
		return r.MinK
	}
	if ct > r.MaxK {
		return r.MaxK
	}
	return ct
}
