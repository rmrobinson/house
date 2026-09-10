package main

import (
	"context"
	"sync"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/bridges/lib/webosctrl"
	"github.com/rmrobinson/house/service/bridge"
)

// deviceConfig describes one webOS TV, statically configured and/or learned
// via SSDP discovery and persisted back to config as fields are learned
// (ClientKey after first pairing, Host as it moves on the network).
type deviceConfig struct {
	// UUID is empty until either configured directly or learned via SSDP -
	// see discover.go's doc comment on why it's the one true device id.
	UUID string `mapstructure:"uuid" yaml:"uuid"`
	// Host is optional to configure directly; SSDP discovery is the source
	// of truth and will correct a stale value.
	Host string `mapstructure:"host" yaml:"host"`
	Name string `mapstructure:"name" yaml:"name"`
	// MAC enables Wake-on-LAN for waking a device in full standby (socket
	// closed). Not learned automatically - SSDP responses don't carry a MAC
	// - so this is a manual one-time config entry (see TODO.md's "assign IP
	// to MAC map for LG TV").
	MAC string `mapstructure:"mac" yaml:"mac"`
	// HasTuner gates whether the Channel trait is populated at all. Per the
	// handoff's recon item 4, a tuner-less (HDMI/ARC-only) setup may behave
	// unpredictably against ssap://tv/... endpoints, so this defaults to
	// false and must be opted into per-device.
	HasTuner bool `mapstructure:"has_tuner" yaml:"has_tuner"`
	// ClientKey is populated automatically once pairing succeeds and
	// persisted back to config - see persistClientKey. Never set this by
	// hand except to force a fresh pairing prompt by clearing it.
	//
	// Without an explicit yaml tag here, viper.WriteConfig marshals this
	// struct through yaml.Marshal's default field-name lowercasing
	// ("clientkey"/"hastuner" above), ignoring the mapstructure tag entirely
	// - so what gets written never matches what UnmarshalKey's mapstructure
	// decode later looks for. Confirmed live: every bridge restart re-paired
	// from scratch instead of reusing the persisted key, because the key it
	// wrote as "clientkey" never matched back to this field on read.
	ClientKey string `mapstructure:"client_key" yaml:"client_key"`
}

// webosDevice is one configured/discovered device's identity plus its live
// session. cancel stops that session's Run goroutine - used when the
// device's host changes (a new Session must be dialed against the new
// address) or the bridge shuts down.
type webosDevice struct {
	cfg     deviceConfig
	session webosSession
	cancel  context.CancelFunc
}

// WebOSBridge is the bridge.Handler implementation for LG webOS TVs. Each
// discovered/configured device gets its own webosctrl.Session with an
// independent reconnect lifecycle, modeled on bridges/cast's per-device
// sessions.
type WebOSBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	mu      sync.Mutex
	devices map[string]*webosDevice // keyed by webos uuid == Device.Id
	// pending holds config entries pre-seeded with a Host but no UUID yet -
	// see webos.example.yaml's documented "pin a name/mac/has_tuner before
	// the device is ever discovered" workflow. A device has no uuid until
	// SSDP tells us one, so these can't be keyed into devices yet; discovery
	// matches them by Host (matchPendingLocked) and promotes them into
	// devices once a uuid is known. Included in deviceConfigsLocked so an
	// unmatched entry survives a config rewrite rather than being dropped.
	pending []deviceConfig
}

// NewWebOSBridge creates a bridge for the supplied set of statically
// configured/persisted devices. Sessions for devices with a known Host are
// started once Run is called; devices with no Host yet wait for SSDP
// discovery to supply one.
func NewWebOSBridge(logger *zap.Logger, svc *bridge.Service, configs []deviceConfig) *WebOSBridge {
	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "WEBOSB1",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
		},
	}

	wb := &WebOSBridge{
		logger:  logger,
		svc:     svc,
		b:       b,
		devices: make(map[string]*webosDevice),
	}

	for _, cfg := range configs {
		if cfg.UUID == "" {
			if cfg.Host == "" {
				logger.Warn("ignoring configured webos device with no uuid or host", zap.String("name", cfg.Name))
				continue
			}
			// No uuid to key a session on yet - held aside until discovery
			// reports a device at this Host, at which point its uuid,
			// mac, name and has_tuner are merged in (see discoverOnce).
			wb.pending = append(wb.pending, cfg)
			continue
		}
		wb.devices[cfg.UUID] = &webosDevice{cfg: cfg}
	}

	return wb
}

// Run starts discovery and every configured/discovered device's session. It
// blocks until ctx is done.
func (wb *WebOSBridge) Run(ctx context.Context) {
	wb.mu.Lock()
	for uuid, wd := range wb.devices {
		if wd.cfg.Host != "" {
			wb.startLocked(uuid, wd)
		}
	}
	wb.mu.Unlock()

	wb.discoverLoop(ctx)
}

// discoverLoop re-scans SSDP on an interval for the lifetime of ctx, merging
// results into wb.devices by uuid. Unlike bridges/cast's opt-in-once mDNS,
// this runs unconditionally: there's no way to pre-populate an unpaired
// device's client-key, so discovery is core to this bridge's operation, not
// an optional convenience (see the plan doc).
func (wb *WebOSBridge) discoverLoop(ctx context.Context) {
	interval := time.Duration(viper.GetInt("bridge.refresh_interval")) * time.Second

	wb.discoverOnce()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			wb.discoverOnce()
		case <-ctx.Done():
			wb.logger.Info("run context cancelled")
			return
		}
	}
}

func (wb *WebOSBridge) discoverOnce() {
	found := discoverDevices(wb.logger, ssdpSearch)
	wb.mergeDiscovered(found)
}

// mergeDiscovered folds found into wb.devices/wb.pending: known uuids get
// their Host refreshed (restarting the session if it changed), unknown ones
// are matched against a pending pre-seeded entry by Host (if any) and
// started as a new device. Split out from discoverOnce so the merge logic
// can be tested against a hand-built found list without a real SSDP call.
func (wb *WebOSBridge) mergeDiscovered(found []discovered) {
	wb.mu.Lock()
	matched := false
	for _, d := range found {
		wd, known := wb.devices[d.UUID]
		if !known {
			cfg := deviceConfig{UUID: d.UUID, Host: d.Host}
			if i := wb.matchPendingLocked(d.Host); i >= 0 {
				pre := wb.pending[i]
				cfg.Name = pre.Name
				cfg.MAC = pre.MAC
				cfg.HasTuner = pre.HasTuner
				wb.pending = append(wb.pending[:i], wb.pending[i+1:]...)
				matched = true
				wb.logger.Info("matched pre-seeded webos config entry to discovered device",
					zap.String("uuid", d.UUID), zap.String("host", d.Host))
			}
			wd = &webosDevice{cfg: cfg}
			wb.devices[d.UUID] = wd
			wb.startLocked(d.UUID, wd)
			continue
		}

		if wd.cfg.Host != d.Host {
			wb.logger.Info("webos device host changed, reconnecting",
				zap.String("uuid", d.UUID), zap.String("old_host", wd.cfg.Host), zap.String("new_host", d.Host))
			wd.cfg.Host = d.Host
			if wd.cancel != nil {
				wd.cancel()
			}
			wb.startLocked(d.UUID, wd)
		}
	}
	configs := wb.deviceConfigsLocked()
	wb.mu.Unlock()

	if matched {
		// Persist immediately so the uuid<->mac/name/has_tuner association
		// survives a restart even if the process stops before this device
		// ever finishes pairing (persistClientKey wouldn't run yet).
		wb.persistDeviceConfigs(configs)
	}
}

// matchPendingLocked returns the index into wb.pending of a pre-seeded
// config entry whose Host matches host, or -1 if none does. Caller must
// hold wb.mu.
func (wb *WebOSBridge) matchPendingLocked(host string) int {
	for i, p := range wb.pending {
		if p.Host == host {
			return i
		}
	}
	return -1
}

// startLocked (re)starts wd's session against its current cfg.Host. Caller
// must hold wb.mu.
func (wb *WebOSBridge) startLocked(uuid string, wd *webosDevice) {
	logger := wb.logger.With(zap.String("device_id", uuid), zap.String("host", wd.cfg.Host))

	sess := webosctrl.NewSession(
		logger,
		wd.cfg.Host,
		wd.cfg.HasTuner,
		func() string { return wd.cfg.ClientKey },
		func(clientKey string) { wb.persistClientKey(uuid, clientKey) },
		func(st webosctrl.Status) { wb.handleStatus(uuid, st) },
	)
	wd.session = sess

	ctx, cancel := context.WithCancel(context.Background())
	wd.cancel = cancel
	go sess.Run(ctx)
}

// persistClientKey saves a freshly issued client-key back to config, keyed
// by uuid, so the next process start reconnects without re-prompting.
func (wb *WebOSBridge) persistClientKey(uuid, clientKey string) {
	wb.mu.Lock()
	wd, ok := wb.devices[uuid]
	if ok {
		wd.cfg.ClientKey = clientKey
	}
	configs := wb.deviceConfigsLocked()
	wb.mu.Unlock()

	if !ok {
		return
	}

	wb.logger.Info("webos device paired, persisting client key", zap.String("device_id", uuid))
	wb.persistDeviceConfigs(configs)
}

// persistDeviceConfigs writes configs back as the whole webos.devices list
// (viper has no notion of updating a single list element).
func (wb *WebOSBridge) persistDeviceConfigs(configs []deviceConfig) {
	viper.Set("webos.devices", configs)
	if err := viper.WriteConfig(); err != nil {
		wb.logger.Error("unable to persist webos device configs", zap.Error(err))
	}
}

// deviceConfigsLocked returns every known device's current config -
// paired/discovered devices plus any still-unmatched pending entries - for
// persisting back as a whole. Pending entries must be included or a config
// rewrite (e.g. persistClientKey firing for an unrelated device) would
// silently drop any pre-seeded entry discovery hasn't matched yet. Caller
// must hold wb.mu.
func (wb *WebOSBridge) deviceConfigsLocked() []deviceConfig {
	configs := make([]deviceConfig, 0, len(wb.devices)+len(wb.pending))
	for _, wd := range wb.devices {
		configs = append(configs, wd.cfg)
	}
	configs = append(configs, wb.pending...)
	return configs
}

func (wb *WebOSBridge) handleStatus(uuid string, st webosctrl.Status) {
	wb.mu.Lock()
	wd, ok := wb.devices[uuid]
	var cfg deviceConfig
	if ok {
		cfg = wd.cfg
	}
	wb.mu.Unlock()
	if !ok {
		return
	}

	wb.svc.UpdateDevice(normalize(cfg, st))
}

// lookupDevice returns a private snapshot of the named device - a copy, not
// the shared *webosDevice held in wb.devices - so command dispatch (which
// runs outside wb.mu, sometimes from a ProcessCommandAsync goroutine that
// outlives this call entirely) never races with mergeDiscovered/startLocked
// reassigning wd.session/wd.cfg out from under it on a concurrent host
// change. cfg is a plain value and session is an interface value (a stable
// reference to the *Session, itself safe for concurrent use per its own doc
// comment), so a shallow copy is sufficient.
func (wb *WebOSBridge) lookupDevice(id string) (*webosDevice, error) {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	wd, ok := wb.devices[id]
	if !ok || wd.session == nil {
		return nil, bridge.ErrDeviceNotFound
	}
	snapshot := *wd
	return &snapshot, nil
}

// ProcessCommand takes a given command request and attempts to execute it,
// blocking for the device's response (bounded by commandTimeout, defined in
// command.go).
func (wb *WebOSBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	wd, err := wb.lookupDevice(cmd.GetDeviceId())
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	if err := dispatchCommand(ctx, wd, cmd); err != nil {
		return nil, err
	}
	return normalize(wd.cfg, wd.session.Status()), nil
}

// ProcessCommandAsync dispatches cmd in the background and reports its
// outcome via Service.CompleteCommand once the device responds (or the
// command times out).
func (wb *WebOSBridge) ProcessCommandAsync(ctx context.Context, cmd *command.Command) error {
	wd, err := wb.lookupDevice(cmd.GetDeviceId())
	if err != nil {
		return err
	}

	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()

		err := dispatchCommand(bgCtx, wd, cmd)
		var resultDevice *device.Device
		if err == nil {
			resultDevice = normalize(wd.cfg, wd.session.Status())
		}
		wb.svc.CompleteCommand(cmd, err, resultDevice)
	}()

	return nil
}

// Refresh is present to conform to the bridge.Handler interface. This bridge
// is push-driven (subscriptions) plus its own discovery ticker - there's no
// separate polling loop needed here.
func (wb *WebOSBridge) Refresh(ctx context.Context) error {
	return nil
}

// SetBridgeConfig takes the supplied config params and saves them for future
// reference.
func (wb *WebOSBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	wb.b.Config.Name = config.Name
	wb.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	return viper.WriteConfig()
}
