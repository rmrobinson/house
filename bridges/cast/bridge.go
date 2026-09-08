package main

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/spf13/viper"

	"google.golang.org/protobuf/proto"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/bridges/lib/castctrl"
	"github.com/rmrobinson/house/service/bridge"
)

// deviceConfig describes one statically-configured Cast device. mDNS
// discovery (which would let Kind be inferred from the "ca" TXT record via
// DeviceKind) isn't implemented in this bridge — Kind is set directly here
// instead, since the fleet it was built against already has a known,
// unchanging classification (see devices.go's DeviceKind doc comment).
type deviceConfig struct {
	UUID string `mapstructure:"uuid"`
	Host string `mapstructure:"host"`
	// Name defaults to Host if unset. There's no mDNS friendly name to fall
	// back to without discovery, and UpdateDeviceConfig — which would let a
	// client rename the device after the fact — isn't implemented anywhere
	// in house yet.
	Name string `mapstructure:"name"`
	// Kind is "media_player" (default) or "television".
	Kind string `mapstructure:"kind"`
}

// castSession is the subset of *castctrl.Session that command dispatch
// needs. Defined as an interface (satisfied implicitly by *castctrl.Session)
// so dispatchCommand/setVolumeAbsolute can be tested against a fake without
// a live TLS connection to a device.
type castSession interface {
	Run(ctx context.Context)
	Status() castctrl.Status
	Play(ctx context.Context) error
	Pause(ctx context.Context) error
	StopMedia(ctx context.Context) error
	SeekAbsolute(ctx context.Context, positionS float64) error
	SetVolumeLevel(ctx context.Context, level float64) error
	SetMuted(ctx context.Context, muted bool) error
	LaunchApp(ctx context.Context, appID string) error
}

// castDevice is one configured device's static identity plus its live
// session.
type castDevice struct {
	uuid string
	addr string
	kind Kind
	name string

	session castSession
}

// CastBridge is the bridge.Handler implementation for Google Cast devices.
// Each configured device gets its own castctrl.Session with an independent
// reconnect lifecycle, modeled on bridges/esphome's per-node supervisor —
// one device's connection trouble must not affect another's.
type CastBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	mu       sync.Mutex
	devices  map[string]*castDevice // keyed by Device.Id (Cast UUID)
	artProxy *ArtProxy              // nil means art URLs aren't rewritten
}

// NewCastBridge creates a bridge for the supplied set of statically
// configured Cast devices. Sessions aren't connected until Start is called.
func NewCastBridge(logger *zap.Logger, svc *bridge.Service, configs []deviceConfig) *CastBridge {
	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "CASTB1",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	cb := &CastBridge{
		logger:  logger,
		svc:     svc,
		b:       b,
		devices: make(map[string]*castDevice),
	}

	for _, cfg := range configs {
		cb.addDevice(cfg)
	}

	return cb
}

func (cb *CastBridge) addDevice(cfg deviceConfig) {
	name := cfg.Name
	if name == "" {
		name = cfg.Host
	}

	cd := &castDevice{
		uuid: cfg.UUID,
		addr: cfg.Host + ":8009",
		kind: ParseKind(cfg.Kind),
		name: name,
	}

	logger := cb.logger.With(zap.String("device_id", cfg.UUID), zap.String("host", cfg.Host))
	cd.session = castctrl.NewSession(logger, cd.addr, func(status castctrl.Status) {
		cb.handleStatus(cd, status)
	})

	cb.mu.Lock()
	cb.devices[cfg.UUID] = cd
	cb.mu.Unlock()
}

// SetArtProxy attaches an art proxy; every device's art URLs are rewritten
// to point at it from the next status change onward. Optional — nil (the
// default) leaves devices reporting the original upstream URLs.
func (cb *CastBridge) SetArtProxy(proxy *ArtProxy) {
	cb.mu.Lock()
	cb.artProxy = proxy
	cb.mu.Unlock()
}

func (cb *CastBridge) handleStatus(cd *castDevice, status castctrl.Status) {
	cb.svc.UpdateDevice(cb.normalizeCurrent(cd, status))
}

// normalizeCurrent builds the current device.Device for cd from an already-
// obtained Status snapshot (avoiding a second, possibly inconsistent read of
// cd.session.Status() when the caller already has one in hand).
func (cb *CastBridge) normalizeCurrent(cd *castDevice, status castctrl.Status) *device.Device {
	d := Normalize(time.Now(), NormalizeOptions{
		DeviceID: cd.uuid,
		Address:  cd.addr,
		Kind:     cd.kind,
		Name:     cd.name,
	}, status)

	cb.mu.Lock()
	artProxy := cb.artProxy
	cb.mu.Unlock()
	if artProxy != nil {
		rewriteArtURLs(d, artProxy)
	}

	d.Version = ComputeVersion(d)
	return d
}

// Start connects every configured device. Each session connects and
// reconnects independently in the background; Start returns once the
// connection goroutines have been launched, not once they're ready.
func (cb *CastBridge) Start(ctx context.Context) {
	cb.mu.Lock()
	devices := make([]*castDevice, 0, len(cb.devices))
	for _, cd := range cb.devices {
		devices = append(devices, cd)
	}
	cb.mu.Unlock()

	for _, cd := range devices {
		go cd.session.Run(ctx)
	}
}

func (cb *CastBridge) lookupDevice(id string) (*castDevice, error) {
	cb.mu.Lock()
	cd, ok := cb.devices[id]
	cb.mu.Unlock()
	if !ok {
		return nil, bridge.ErrDeviceNotFound
	}
	return cd, nil
}

// ProcessCommand takes a given command request and attempts to execute it,
// blocking for the device's response (bounded by commandTimeout).
func (cb *CastBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	cd, err := cb.lookupDevice(cmd.GetDeviceId())
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, commandDeadline(cd, cmd))
	defer cancel()

	return cb.executeCommand(ctx, cd, cmd)
}

// ProcessCommandAsync dispatches cmd in the background and reports its
// outcome via Service.CompleteCommand once the device responds (or the
// command times out). It uses its own bounded context rather than the
// caller's — ctx belongs to the ExecuteCommandAsync RPC call, which returns
// (and cancels ctx) well before the device responds.
func (cb *CastBridge) ProcessCommandAsync(ctx context.Context, cmd *command.Command) error {
	cd, err := cb.lookupDevice(cmd.GetDeviceId())
	if err != nil {
		return err
	}

	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), commandDeadline(cd, cmd))
		defer cancel()

		d, err := cb.executeCommand(bgCtx, cd, cmd)
		cb.svc.CompleteCommand(cmd, err, d)
	}()

	return nil
}

// Refresh is present to conform to the bridge.Handler interface. This bridge
// is push-driven — Cast devices push RECEIVER_STATUS/MEDIA_STATUS on every
// change — so there's no polling loop needed here.
func (cb *CastBridge) Refresh(ctx context.Context) error {
	return nil
}

// rewriteArtURLs replaces every populated art URL in d's media details with
// a URL served by proxy, so clients never fetch the upstream URL directly.
func rewriteArtURLs(d *device.Device, proxy *ArtProxy) {
	var media *trait.Media
	if mp := d.GetMediaPlayer(); mp != nil {
		media = mp.GetMedia()
	} else if tv := d.GetTelevision(); tv != nil {
		media = tv.GetMedia()
	}

	state := media.GetState()
	if state == nil {
		return
	}

	if sd := state.GetSongDetails(); sd != nil && sd.GetAlbumArtUrl() != "" {
		sd.AlbumArtUrl = proto.String(proxy.ProxyURL(sd.GetAlbumArtUrl()))
	}
	if md := state.GetMovieDetails(); md != nil && md.GetArtUrl() != "" {
		md.ArtUrl = proto.String(proxy.ProxyURL(md.GetArtUrl()))
	}
	if sd := state.GetShowDetails(); sd != nil && sd.GetArtUrl() != "" {
		sd.ArtUrl = proto.String(proxy.ProxyURL(sd.GetArtUrl()))
	}
	if gd := state.GetGenericDetails(); gd != nil && gd.GetArtUrl() != "" {
		gd.ArtUrl = proto.String(proxy.ProxyURL(gd.GetArtUrl()))
	}
}

// SetBridgeConfig takes the supplied config params and saves them for future
// reference.
func (cb *CastBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	cb.b.Config.Name = config.Name
	cb.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}
