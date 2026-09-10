package webosctrl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// commandTimeout bounds how long a single ssap:// request waits for its
// correlated response.
const commandTimeout = 5 * time.Second

// registerTimeout bounds how long a first-time pairing waits for a human to
// accept (or ignore) the on-screen prompt. Deliberately generous - this is a
// one-time, human-in-the-loop event, not a routine command.
const registerTimeout = 60 * time.Second

var (
	// ErrPairingRequired is returned by command methods when no client-key is
	// held yet and the register handshake hasn't completed. The caller
	// (bridges/webos) surfaces this as "device not ready" rather than
	// retrying blindly - pairing requires a human to accept an on-screen
	// prompt, which Session cannot do on its own.
	ErrPairingRequired = errors.New("webosctrl: device not paired yet")
	// ErrRegisterTimeout is returned when a human doesn't accept the
	// on-screen pairing prompt within registerTimeout.
	ErrRegisterTimeout = errors.New("webosctrl: register handshake timed out waiting for on-screen accept")
	// ErrRequestRejected is returned when a device answers a request with
	// returnValue: false.
	ErrRequestRejected = errors.New("webosctrl: device rejected request")
)

// PowerState is the coarse power state derived from the power subscription.
// It exists purely as an internal signal for Session/normalize logic - it is
// intentionally not surfaced as a new API-level enum (see the plan doc: the
// existing OnOff trait plus Device.Address.is_reachable already express
// everything a client needs).
type PowerState int

const (
	PowerUnknown PowerState = iota
	PowerActive
	PowerScreenOff // "Screen Off"/"Active Standby"/etc - socket alive, screen not
)

// ParsePowerState maps the free-form state string webOS reports into the
// coarse PowerState Session tracks. The exact set of strings a given TV
// reports (and whether "Active Standby" is even distinguishable from
// "Screen Off" this way) is a recon item - see the handoff/plan.
func ParsePowerState(s string) PowerState {
	switch s {
	case "Active":
		return PowerActive
	case "":
		return PowerUnknown
	default:
		return PowerScreenOff
	}
}

// App is one entry in the device's installed-application list.
type App struct {
	ID      string
	Name    string
	Version string
}

// Channel is the currently tuned channel.
type Channel struct {
	ID     string
	Number string
	Name   string
}

// Status is a point-in-time snapshot of everything a Session knows about its
// webOS TV. webosctrl has no dependency on house's api/device proto -
// bridges/webos's normalizer converts this into a device.Device.
type Status struct {
	// Reachable reflects the underlying websocket transport, independent of
	// pairing - a freshly discovered, never-paired device is Reachable but
	// not Paired until a human accepts the on-screen prompt. Full standby
	// (socket closed) is Reachable: false; active-standby/screen-off (socket
	// alive, screen not) is Reachable: true - this is what lets
	// bridges/webos distinguish the two without a bespoke power_state enum,
	// per the plan doc's state-mapping table.
	Reachable bool
	Paired    bool // false until the register handshake has completed at least once

	// EverPaired is set the first time Paired goes true and, unlike Paired,
	// is never cleared again for this Session's lifetime - it distinguishes
	// "never successfully paired" (normalize should show no state at all)
	// from "was paired, now disconnected" (normalize should keep showing the
	// last-known state, per the plan doc's state-mapping table). Paired
	// itself must still drop to false on disconnect: request() gates live
	// command sends on it, since a lost connection genuinely can't accept
	// requests regardless of pairing history.
	EverPaired bool

	Power PowerState

	Volume int32
	Muted  bool

	Apps         []App // installed apps, from listApps; empty if not yet fetched
	ForegroundID string

	// HasChannel is true once a channel status push has actually been
	// received - it's the live-confirmed counterpart to the bridge's static
	// has_tuner config, distinguishing "device has a tuner but hasn't
	// reported yet" from "device has no tuner" so normalize doesn't surface
	// an empty ChannelDetails before any real data exists.
	HasChannel bool
	Channel    Channel
}

// Session is the per-device webOS state machine, built on top of a Conn. It
// runs the register handshake on every (re)connect, re-establishes its
// subscriptions (volume, power, foreground app, channel), and exposes
// command methods that issue a single ssap:// request each.
//
// All exported methods are safe for concurrent use.
type Session struct {
	logger *zap.Logger
	conn   *Conn

	getClientKey func() string          // reads the persisted client-key, if any
	onPaired     func(clientKey string) // called once with a freshly issued client-key
	onChange     func(Status)

	hasTuner bool // from config - see bridges/webos's deviceConfig

	mu     sync.Mutex
	status Status
}

// NewSession creates a Session for the TV at host. The connection isn't
// established until Run is called. getClientKey is consulted on every
// (re)connect; onPaired is called (at most once per process, typically) the
// first time a register handshake returns a fresh client-key, so the caller
// can persist it.
func NewSession(logger *zap.Logger, host string, hasTuner bool, getClientKey func() string, onPaired func(string), onChange func(Status)) *Session {
	s := &Session{
		logger:       logger,
		getClientKey: getClientKey,
		onPaired:     onPaired,
		onChange:     onChange,
		hasTuner:     hasTuner,
	}
	s.conn = newConn(logger, host, dialTV, s.handleStateChange)
	return s
}

// Run dials and maintains the underlying connection, running the register
// handshake and (re)establishing subscriptions on every (re)connect, until
// ctx is done. It blocks until then - call it from its own goroutine.
func (s *Session) Run(ctx context.Context) {
	s.conn.Run(ctx)
}

// Status returns the current snapshot.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Session) handleStateChange(connected bool) {
	if !connected {
		s.mu.Lock()
		s.status.Reachable = false
		s.status.Paired = false
		s.mu.Unlock()
		s.notify()
		return
	}

	s.mu.Lock()
	s.status.Reachable = true
	s.mu.Unlock()
	s.notify()

	// Run the handshake/subscription setup in the background: onStateChange
	// is called synchronously from Conn's read-loop goroutine setup path, and
	// blocking here would delay Conn actually starting to read.
	go s.onConnected()
}

func (s *Session) onConnected() {
	ctx, cancel := context.WithTimeout(context.Background(), registerTimeout)
	defer cancel()

	clientKey := s.getClientKey()
	newKey, err := s.register(ctx, clientKey)
	if err != nil {
		s.logger.Warn("register handshake failed", zap.Error(err))
		s.conn.Close() // treat as connection loss; Run will reconnect and retry
		return
	}
	if newKey != "" && newKey != clientKey {
		s.onPaired(newKey)
	}

	s.mu.Lock()
	s.status.Paired = true
	s.status.EverPaired = true
	s.mu.Unlock()

	s.subscribeAll()
	s.notify()
}

func (s *Session) notify() {
	if s.onChange != nil {
		s.onChange(s.Status())
	}
}

// register runs the register handshake, returning the client-key to persist
// (unchanged from clientKey if the TV didn't issue a new one). It blocks
// until "registered" is received, ctx is done, or the connection is lost -
// on first pairing this is however long a human takes to accept the
// on-screen prompt.
func (s *Session) register(ctx context.Context, clientKey string) (string, error) {
	id := s.conn.NextRequestID()
	ch, cancel := s.conn.await(id)
	defer cancel()

	req := requestEnvelope{Type: "register", ID: id, Payload: newRegisterPayload(clientKey)}
	if err := s.conn.send(req); err != nil {
		return "", err
	}

	done, _ := s.conn.connectedDone()
	loggedPrompt := false
	for {
		select {
		case e := <-ch:
			switch e.Type {
			case "registered":
				var p registeredPayload
				if err := json.Unmarshal(e.Payload, &p); err != nil {
					return "", fmt.Errorf("decoding registered payload: %w", err)
				}
				return p.ClientKey, nil
			case "error":
				return "", fmt.Errorf("%w: %s", ErrRequestRejected, e.Error)
			default:
				// Intermediate "response" while pairingType PROMPT is shown
				// on-screen - not terminal, keep waiting.
				if !loggedPrompt {
					s.logger.Info("waiting for on-screen pairing accept")
					loggedPrompt = true
				}
			}
		case <-done:
			return "", errConnectionLost
		case <-ctx.Done():
			return "", ErrRegisterTimeout
		}
	}
}

func (s *Session) subscribeAll() {
	s.subscribe(uriGetVolume, func(e envelope) {
		var p volumePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			s.logger.Warn("unable to decode volume push", zap.Error(err))
			return
		}
		s.mu.Lock()
		s.status.Volume = p.Volume
		s.status.Muted = p.Muted
		s.mu.Unlock()
		s.notify()
	})

	s.subscribe(uriGetPower, func(e envelope) {
		var p powerPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			s.logger.Warn("unable to decode power push", zap.Error(err))
			return
		}
		s.mu.Lock()
		s.status.Power = ParsePowerState(p.State)
		s.mu.Unlock()
		s.notify()
	})

	s.subscribe(uriForegroundApp, func(e envelope) {
		var p foregroundAppPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			s.logger.Warn("unable to decode foreground app push", zap.Error(err))
			return
		}
		s.mu.Lock()
		s.status.ForegroundID = p.AppID
		s.mu.Unlock()
		s.notify()
	})

	if s.hasTuner {
		s.subscribe(uriGetChannel, func(e envelope) {
			var p channelPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				s.logger.Warn("unable to decode channel push", zap.Error(err))
				return
			}
			s.mu.Lock()
			s.status.HasChannel = true
			s.status.Channel = Channel{ID: p.ChannelID, Number: p.ChannelNumber, Name: p.ChannelName}
			s.mu.Unlock()
			s.notify()
		})
	}

	// listApps isn't a subscription - refresh it once per connect. Installed
	// apps change rarely enough that re-fetching on every reconnect (rather
	// than polling) is sufficient.
	go s.refreshApps()
}

func (s *Session) subscribe(uri string, handler func(envelope)) {
	id := s.conn.NextRequestID()
	s.conn.subscribe(id, handler)
	req := requestEnvelope{Type: "subscribe", ID: id, URI: uri}
	if err := s.conn.send(req); err != nil {
		s.logger.Warn("unable to send subscribe request", zap.String("uri", uri), zap.Error(err))
	}
}

func (s *Session) refreshApps() {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	e, err := s.request(ctx, uriListApps, nil)
	if err != nil {
		s.logger.Warn("unable to fetch installed apps", zap.Error(err))
		return
	}
	var p listAppsPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		s.logger.Warn("unable to decode installed apps", zap.Error(err))
		return
	}

	apps := make([]App, 0, len(p.Apps))
	for _, a := range p.Apps {
		apps = append(apps, App{ID: a.ID, Name: a.Title, Version: a.Version})
	}

	s.mu.Lock()
	s.status.Apps = apps
	s.mu.Unlock()
	s.notify()
}

// request issues a single ssap:// request/response, decoding the common
// returnValue shape and turning returnValue:false into ErrRequestRejected.
func (s *Session) request(ctx context.Context, uri string, payload any) (envelope, error) {
	if !s.Status().Paired {
		return envelope{}, ErrPairingRequired
	}

	req := requestEnvelope{Type: "request", ID: s.conn.NextRequestID(), URI: uri, Payload: payload}
	e, err := s.conn.SendRequest(ctx, req)
	if err != nil {
		return envelope{}, err
	}
	if e.Type == "error" {
		return envelope{}, fmt.Errorf("%w: %s", ErrRequestRejected, e.Error)
	}

	// Peek at returnValue without requiring every caller to redeclare it -
	// commands with no further payload (mute, close, channel step) only care
	// about this.
	var rv returnValuePayload
	if err := json.Unmarshal(e.Payload, &rv); err == nil && !rv.ReturnValue && rv.ErrorText != "" {
		return envelope{}, fmt.Errorf("%w: %s", ErrRequestRejected, rv.ErrorText)
	}
	return e, nil
}

// SetVolumeAbsolute sets the device volume to level (0-100).
func (s *Session) SetVolumeAbsolute(ctx context.Context, level int32) error {
	_, err := s.request(ctx, uriSetVolume, setVolumePayload{Volume: level})
	return err
}

// SetMuted mutes or unmutes the device without changing its volume level.
func (s *Session) SetMuted(ctx context.Context, muted bool) error {
	_, err := s.request(ctx, uriSetMute, setMutePayload{Mute: muted})
	return err
}

// LaunchApp launches the application with the given id.
func (s *Session) LaunchApp(ctx context.Context, appID string) error {
	_, err := s.request(ctx, uriLaunchApp, launchAppPayload{ID: appID})
	return err
}

// CloseApp closes the given foreground application, returning the device to
// its home/idle state.
func (s *Session) CloseApp(ctx context.Context, appID string) error {
	_, err := s.request(ctx, uriCloseApp, launchAppPayload{ID: appID})
	return err
}

// SetChannel tunes directly to the given channel number.
func (s *Session) SetChannel(ctx context.Context, channelNumber string) error {
	_, err := s.request(ctx, uriOpenChannel, openChannelPayload{ChannelNumber: channelNumber})
	return err
}

// ChannelStep steps the tuner up (positive) or down (negative) delta times.
func (s *Session) ChannelStep(ctx context.Context, delta int32) error {
	uri := uriChannelUp
	if delta < 0 {
		uri = uriChannelDown
		delta = -delta
	}
	for i := int32(0); i < delta; i++ {
		if _, err := s.request(ctx, uri, nil); err != nil {
			return err
		}
	}
	return nil
}

// TurnOnScreen requests the screen turn on. The caller (bridges/webos's
// command dispatch) is responsible for Wake-on-LAN when the device isn't
// currently reachable at all - Session only knows about its own live
// connection, not the wider reconnect/discovery state.
func (s *Session) TurnOnScreen(ctx context.Context) error {
	_, err := s.request(ctx, uriTurnOnScreen, nil)
	return err
}

// TurnOffScreen requests the screen turn off. The socket is expected to stay
// alive - this is a lower-latency, reversible state, not a suspend.
func (s *Session) TurnOffScreen(ctx context.Context) error {
	_, err := s.request(ctx, uriTurnOffScreen, nil)
	return err
}

// PowerOff requests a full system standby (socket closes). Unlike
// TurnOffScreen, this is universally supported across firmware builds -
// verified as the fallback for TVs whose com.webos.service.tvpower service
// doesn't expose turnOffScreen (confirmed 404 on at least one real webOS
// 5.5 build, see bridges/webos/README.md).
func (s *Session) PowerOff(ctx context.Context) error {
	_, err := s.request(ctx, uriSystemTurnOff, nil)
	return err
}
