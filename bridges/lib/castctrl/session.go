package castctrl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// internalRequestTimeout bounds requests Session issues on its own
// initiative (refreshing status after a reconnect or app change), which have
// no caller-supplied context to inherit a deadline from.
const internalRequestTimeout = 5 * time.Second

// SessionState is the CASTV2 session state machine's current state.
type SessionState int

const (
	StateDisconnected SessionState = iota
	StateConnecting
	StateReceiverConnected
	StateAppConnected
)

func (s SessionState) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateReceiverConnected:
		return "receiver_connected"
	case StateAppConnected:
		return "app_connected"
	default:
		return "unknown"
	}
}

var (
	// ErrNotReceiverConnected is returned by write methods when the session
	// hasn't yet reached ReceiverConnected (or later).
	ErrNotReceiverConnected = errors.New("castctrl: not connected to receiver")
	// ErrNoActiveApp is returned by media write methods when no application
	// is running.
	ErrNoActiveApp = errors.New("castctrl: no active app")
	// ErrNoActiveMedia is returned by media write methods when an app is
	// running but has no active media session.
	ErrNoActiveMedia = errors.New("castctrl: no active media session")
	// ErrRequestRejected is returned when a device answers a request with
	// INVALID_REQUEST or LOAD_FAILED instead of the expected status message.
	// Wrapped with the device's reason, if it supplied one.
	ErrRequestRejected = errors.New("castctrl: device rejected request")
)

// errorPayload decodes the "type"/"reason" shape of an INVALID_REQUEST or
// LOAD_FAILED response.
type errorPayload struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}

// asRejection reports whether msg is a terminal error response
// (INVALID_REQUEST, LOAD_FAILED, or LAUNCH_ERROR) rather than the
// status/media message the caller was expecting a reply to — see
// namespaces.go's TypeInvalidRequest/TypeLoadFailed/TypeLaunchError doc
// comment. Every call site that decodes a request's response into a status
// struct must check this first: an error response doesn't have a
// "status"/"status[]" field, so unmarshaling it into
// receiverStatusPayload/mediaStatusPayload directly would silently zero out
// the previously-known state instead of surfacing the failure. Returns nil
// if msg isn't an error response.
func asRejection(msg *CastMessage) error {
	var e errorPayload
	if err := json.Unmarshal([]byte(msg.GetPayloadUtf8()), &e); err != nil {
		return nil
	}
	if e.Type != TypeInvalidRequest && e.Type != TypeLoadFailed && e.Type != TypeLaunchError {
		return nil
	}
	if e.Reason != "" {
		return fmt.Errorf("%w: %s: %s", ErrRequestRejected, e.Type, e.Reason)
	}
	return fmt.Errorf("%w: %s", ErrRequestRejected, e.Type)
}

// Status is a point-in-time snapshot of everything a Session knows about its
// Cast device. castctrl has no dependency on house's api/device proto;
// bridges/cast's normalizer converts this into a device.Device.
type Status struct {
	State SessionState

	Volume ReceiverVolume
	App    ReceiverApplication // zero value if no app is running

	// HasMedia is false whenever there's no active media session — check it
	// before reading Media, whose zero value is otherwise indistinguishable
	// from a real IDLE session.
	HasMedia bool
	Media    MediaStatus
}

// Session is the per-device CASTV2 state machine, built on top of a Conn.
// It tracks the receiver's running application and, once one is running,
// that application's media session — reconnecting the app-level connection
// whenever the receiver reports a new transportId, per the CASTV2 protocol
// rule that a transportId is invalidated on every app launch.
//
// All exported methods are safe for concurrent use.
type Session struct {
	logger *zap.Logger
	conn   *Conn

	onChange func(Status)

	mu             sync.Mutex
	state          SessionState
	appTransportID string
	volume         ReceiverVolume
	app            ReceiverApplication
	hasMedia       bool
	media          MediaStatus
}

// NewSession creates a Session for the device at addr ("host:port"). The
// connection isn't established until Run is called.
func NewSession(logger *zap.Logger, addr string, onChange func(Status)) *Session {
	return newSession(logger, addr, dialTLS, onChange)
}

func newSession(logger *zap.Logger, addr string, dial dialFunc, onChange func(Status)) *Session {
	s := &Session{logger: logger, onChange: onChange}
	s.conn = newConn(logger, addr, dial, s.handleMessage, s.handleConnStateChange)
	return s
}

// Run dials and maintains the underlying connection, running the CASTV2
// session handshake on every (re)connect, until ctx is done. It blocks until
// then — call it from its own goroutine.
func (s *Session) Run(ctx context.Context) {
	s.conn.Run(ctx)
}

// Status returns the current snapshot.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		State:    s.state,
		Volume:   s.volume,
		App:      s.app,
		HasMedia: s.hasMedia,
		Media:    s.media,
	}
}

func (s *Session) notify() {
	if s.onChange != nil {
		s.onChange(s.Status())
	}
}

// handleConnStateChange is Conn's onStateChange callback: transport up/down,
// beneath any Cast-level session semantics.
func (s *Session) handleConnStateChange(connected bool) {
	if !connected {
		s.mu.Lock()
		s.state = StateDisconnected
		s.appTransportID = ""
		s.app = ReceiverApplication{}
		s.hasMedia = false
		s.media = MediaStatus{}
		s.mu.Unlock()
		s.notify()
		return
	}

	s.mu.Lock()
	s.state = StateConnecting
	s.mu.Unlock()

	go s.connectReceiver()
}

func (s *Session) connectReceiver() {
	if err := s.conn.Send(NamespaceConnection, SenderID, ReceiverID, &connectPayload{Type: TypeConnect}); err != nil {
		s.logger.Warn("unable to connect to receiver", zap.Error(err))
		return
	}

	s.mu.Lock()
	s.state = StateReceiverConnected
	s.mu.Unlock()
	s.notify()

	s.refreshReceiverStatus()
}

func (s *Session) refreshReceiverStatus() {
	ctx, cancel := context.WithTimeout(context.Background(), internalRequestTimeout)
	defer cancel()

	id := s.conn.NextRequestID()
	resp, err := s.conn.SendRequest(ctx, NamespaceReceiver, SenderID, ReceiverID, id, &getStatusPayload{Type: TypeGetStatus, RequestID: id})
	if err != nil {
		s.logger.Warn("receiver get_status failed", zap.Error(err))
		return
	}
	if err := asRejection(resp); err != nil {
		s.logger.Warn("receiver get_status rejected", zap.Error(err))
		return
	}

	s.applyReceiverStatus(resp)
}

// applyReceiverStatus decodes and applies a RECEIVER_STATUS message,
// regardless of whether it arrived as a GET_STATUS response or an
// unsolicited push — they're the same shape on the wire. Per CASTV2, a
// transportId different from the one we're currently connected to (including
// "now there is one" or "now there isn't") means the app session changed and
// the app-level connection must be re-established, not just re-queried.
func (s *Session) applyReceiverStatus(msg *CastMessage) {
	var payload receiverStatusPayload
	if err := json.Unmarshal([]byte(msg.GetPayloadUtf8()), &payload); err != nil {
		s.logger.Warn("unable to decode receiver status", zap.Error(err))
		return
	}

	var newApp ReceiverApplication
	if len(payload.Status.Applications) > 0 {
		newApp = payload.Status.Applications[0]
	}

	s.mu.Lock()
	s.volume = payload.Status.Volume
	s.app = newApp
	oldTransportID := s.appTransportID
	needsAppConnect := newApp.TransportID != "" && (newApp.TransportID != oldTransportID || s.state != StateAppConnected)
	needsAppDisconnect := newApp.TransportID == "" && oldTransportID != ""

	if needsAppDisconnect {
		s.appTransportID = ""
		s.hasMedia = false
		s.media = MediaStatus{}
		if s.state == StateAppConnected {
			s.state = StateReceiverConnected
		}
	}
	s.mu.Unlock()

	if needsAppConnect {
		s.connectApp(oldTransportID, newApp.TransportID)
		return
	}

	s.notify()
}

func (s *Session) connectApp(oldTransportID, newTransportID string) {
	if oldTransportID != "" && oldTransportID != newTransportID {
		// Best-effort: the old transportId is invalidated regardless of
		// whether this send succeeds.
		_ = s.conn.Send(NamespaceConnection, SenderID, oldTransportID, &closePayload{Type: TypeClose})
	}

	if err := s.conn.Send(NamespaceConnection, SenderID, newTransportID, &connectPayload{Type: TypeConnect}); err != nil {
		s.logger.Warn("unable to connect to app", zap.String("transport_id", newTransportID), zap.Error(err))
		return
	}

	s.mu.Lock()
	s.appTransportID = newTransportID
	s.state = StateAppConnected
	s.hasMedia = false
	s.media = MediaStatus{}
	s.mu.Unlock()
	s.notify()

	// connectApp can be reached synchronously from Conn's read-loop goroutine
	// (an unsolicited RECEIVER_STATUS push routes straight through
	// applyReceiverStatus into here). refreshMediaStatus blocks on a
	// SendRequest whose response can only be delivered by that same read
	// loop, so it must run on its own goroutine — calling it inline would
	// deadlock the connection for the full request timeout.
	go s.refreshMediaStatus(newTransportID)
}

func (s *Session) refreshMediaStatus(transportID string) {
	ctx, cancel := context.WithTimeout(context.Background(), internalRequestTimeout)
	defer cancel()

	id := s.conn.NextRequestID()
	resp, err := s.conn.SendRequest(ctx, NamespaceMedia, SenderID, transportID, id, &getStatusPayload{Type: TypeGetStatus, RequestID: id})
	if err != nil {
		s.logger.Warn("media get_status failed", zap.Error(err))
		return
	}
	if err := asRejection(resp); err != nil {
		s.logger.Warn("media get_status rejected", zap.Error(err))
		return
	}

	s.applyMediaStatus(transportID, resp)
}

// applyMediaStatus decodes and applies a MEDIA_STATUS message, whether a
// GET_STATUS response or an unsolicited push. transportID is the app
// connection the message was received on (or requested against); a message
// from an app we've since moved on from is discarded rather than resurrecting
// a stale mediaSessionId.
func (s *Session) applyMediaStatus(transportID string, msg *CastMessage) {
	var payload mediaStatusPayload
	if err := json.Unmarshal([]byte(msg.GetPayloadUtf8()), &payload); err != nil {
		s.logger.Warn("unable to decode media status", zap.Error(err))
		return
	}

	s.mu.Lock()
	if s.appTransportID != transportID {
		s.mu.Unlock()
		return
	}

	if len(payload.Status) == 0 {
		s.hasMedia = false
		s.media = MediaStatus{}
	} else {
		newStatus := payload.Status[0]
		// Confirmed against a real Spotify Cast session: the device only
		// sends the nested `media` object (content identity + metadata) on
		// the status push that follows a LOAD. Every later push for the same
		// mediaSessionId — e.g. the ones PLAY/PAUSE/SEEK trigger — omits it
		// entirely, since the loaded content hasn't changed. Carry the
		// previous value forward in that case rather than losing title/
		// artist/art on every subsequent play/pause. Guarded by
		// mediaSessionId match so a genuinely new session (new content) that
		// happens to omit `media` doesn't inherit the old session's stale
		// metadata.
		if newStatus.Media == nil && s.hasMedia && newStatus.MediaSessionID == s.media.MediaSessionID {
			newStatus.Media = s.media.Media
		}
		s.hasMedia = true
		s.media = newStatus
	}
	s.mu.Unlock()

	s.notify()
}

// handleMessage is Conn's onMessage callback: every inbound message that
// isn't heartbeat and isn't a correlated response to a pending SendRequest —
// i.e. unsolicited pushes, plus inbound CLOSE.
func (s *Session) handleMessage(msg *CastMessage) {
	switch msg.GetNamespace() {
	case NamespaceConnection:
		s.handleConnectionMessage(msg)
	case NamespaceReceiver:
		s.applyReceiverStatus(msg)
	case NamespaceMedia:
		// Use the message's actual source, not our currently-tracked
		// appTransportID, so applyMediaStatus's staleness guard can catch a
		// push that arrives from an app connection we've since moved on
		// from — comparing appTransportID against itself would never do so.
		s.applyMediaStatus(msg.GetSourceId(), msg)
	default:
		s.logger.Debug("ignoring message on unhandled namespace", zap.String("namespace", msg.GetNamespace()))
	}
}

// handleConnectionMessage handles CLOSE, the only unsolicited message type
// expected on the connection namespace. A CLOSE from the receiver platform
// and a CLOSE from the current app connection mean different things: the
// former means the whole session is being torn down by the device, the
// latter just means the app session ended (another sender may have already
// started a new one, so a receiver refresh follows).
func (s *Session) handleConnectionMessage(msg *CastMessage) {
	var e envelope
	if err := json.Unmarshal([]byte(msg.GetPayloadUtf8()), &e); err != nil || e.Type != TypeClose {
		return
	}

	s.mu.Lock()
	isReceiverClose := msg.GetSourceId() == ReceiverID
	isAppClose := s.appTransportID != "" && msg.GetSourceId() == s.appTransportID
	s.mu.Unlock()

	switch {
	case isReceiverClose:
		s.logger.Info("device closed receiver connection, forcing reconnect")
		s.conn.Close()
	case isAppClose:
		s.logger.Info("device closed app connection", zap.String("transport_id", msg.GetSourceId()))
		s.mu.Lock()
		s.appTransportID = ""
		s.app = ReceiverApplication{}
		s.hasMedia = false
		s.media = MediaStatus{}
		if s.state == StateAppConnected {
			s.state = StateReceiverConnected
		}
		s.mu.Unlock()
		s.notify()
		// Same deadlock hazard as connectApp above — this handler itself
		// runs on Conn's read-loop goroutine.
		go s.refreshReceiverStatus()
	}
}

// mediaTarget resolves the app transportId and mediaSessionId a media
// command must target, or an error if there's no active app/media session
// to target.
func (s *Session) mediaTarget() (transportID string, mediaSessionID int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateAppConnected {
		return "", 0, ErrNoActiveApp
	}
	if !s.hasMedia {
		return "", 0, ErrNoActiveMedia
	}
	return s.appTransportID, s.media.MediaSessionID, nil
}

// SetVolumeLevel sets the device's absolute volume level, in [0, 1]. Callers
// implementing step-synthesis for controlType "master" devices call this
// once per step; castctrl has no opinion on that policy, only on issuing the
// wire command.
func (s *Session) SetVolumeLevel(ctx context.Context, level float64) error {
	s.mu.Lock()
	connected := s.state == StateReceiverConnected || s.state == StateAppConnected
	s.mu.Unlock()
	if !connected {
		return ErrNotReceiverConnected
	}

	id := s.conn.NextRequestID()
	resp, err := s.conn.SendRequest(ctx, NamespaceReceiver, SenderID, ReceiverID, id, &setVolumePayload{
		Type:      TypeSetVolume,
		RequestID: id,
		Volume:    setVolumeValues{Level: &level},
	})
	if err != nil {
		return err
	}
	if err := asRejection(resp); err != nil {
		return err
	}

	s.applyReceiverStatus(resp)
	return nil
}

// SetMuted sets or clears the device's mute state without changing its
// volume level.
func (s *Session) SetMuted(ctx context.Context, muted bool) error {
	s.mu.Lock()
	connected := s.state == StateReceiverConnected || s.state == StateAppConnected
	s.mu.Unlock()
	if !connected {
		return ErrNotReceiverConnected
	}

	id := s.conn.NextRequestID()
	resp, err := s.conn.SendRequest(ctx, NamespaceReceiver, SenderID, ReceiverID, id, &setVolumePayload{
		Type:      TypeSetVolume,
		RequestID: id,
		Volume:    setVolumeValues{Muted: &muted},
	})
	if err != nil {
		return err
	}
	if err := asRejection(resp); err != nil {
		return err
	}

	s.applyReceiverStatus(resp)
	return nil
}

// LaunchApp requests the receiver start running appID (a Cast receiver app
// ID, e.g. "233637DE" for YouTube). Unlike the media-namespace write methods,
// this only requires a receiver connection — no app needs to be running
// already, since launching one is the point.
//
// The correlated response is a RECEIVER_STATUS reflecting the newly launched
// application; applying it here (rather than waiting for the unsolicited
// push that normally follows) is what drives Session's existing
// transportId-change machinery (applyReceiverStatus) to connect to the new
// app and refresh its media status, the same as if another sender had
// launched it.
func (s *Session) LaunchApp(ctx context.Context, appID string) error {
	s.mu.Lock()
	connected := s.state == StateReceiverConnected || s.state == StateAppConnected
	s.mu.Unlock()
	if !connected {
		return ErrNotReceiverConnected
	}

	id := s.conn.NextRequestID()
	resp, err := s.conn.SendRequest(ctx, NamespaceReceiver, SenderID, ReceiverID, id, &launchPayload{
		Type:      TypeLaunch,
		RequestID: id,
		AppID:     appID,
	})
	if err != nil {
		return err
	}
	if err := asRejection(resp); err != nil {
		return err
	}

	s.applyReceiverStatus(resp)
	return nil
}

func (s *Session) sendPlayback(ctx context.Context, playbackType string) error {
	transportID, mediaSessionID, err := s.mediaTarget()
	if err != nil {
		return err
	}

	id := s.conn.NextRequestID()
	resp, err := s.conn.SendRequest(ctx, NamespaceMedia, SenderID, transportID, id, &playbackPayload{
		Type:           playbackType,
		RequestID:      id,
		MediaSessionID: mediaSessionID,
	})
	if err != nil {
		return err
	}
	if err := asRejection(resp); err != nil {
		return err
	}

	s.applyMediaStatus(transportID, resp)
	return nil
}

// Play resumes playback of the current media session.
func (s *Session) Play(ctx context.Context) error { return s.sendPlayback(ctx, TypePlay) }

// Pause pauses the current media session.
func (s *Session) Pause(ctx context.Context) error { return s.sendPlayback(ctx, TypePause) }

// StopMedia ends the current media session.
func (s *Session) StopMedia(ctx context.Context) error { return s.sendPlayback(ctx, TypeStop) }

// SeekAbsolute moves playback to positionS seconds from the start. Callers
// implementing SeekRelative compute the target position themselves from
// Status().Media.CurrentTime and call this.
//
// Per a documented Cast quirk, the echoed status sometimes still reports the
// pre-seek position — receiving the correlated response at all is treated as
// success here; the subsequent unsolicited MEDIA_STATUS corrects the
// position.
//
// positionS isn't validated against the current track's duration — confirmed
// live against a real Chromecast/Spotify session: SeekAbsolute(99999) on a
// 165s track wasn't rejected (no INVALID_REQUEST) or clamped to the track's
// end; the device silently advanced to a different track in the queue and
// reported a position near 0 on that track instead. Callers that want to stay
// within the current track must clamp positionS to the known
// PlaybackLengthS themselves before calling this (as dispatchCommand's
// SeekRelative case already does in bridges/cast/command.go) — this method
// won't do it for them.
func (s *Session) SeekAbsolute(ctx context.Context, positionS float64) error {
	transportID, mediaSessionID, err := s.mediaTarget()
	if err != nil {
		return err
	}

	id := s.conn.NextRequestID()
	resp, err := s.conn.SendRequest(ctx, NamespaceMedia, SenderID, transportID, id, &seekPayload{
		Type:           TypeSeek,
		RequestID:      id,
		MediaSessionID: mediaSessionID,
		CurrentTime:    &positionS,
	})
	if err != nil {
		return err
	}
	if err := asRejection(resp); err != nil {
		return err
	}

	s.applyMediaStatus(transportID, resp)
	return nil
}
