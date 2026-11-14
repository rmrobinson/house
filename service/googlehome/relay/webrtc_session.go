package relay

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"go.uber.org/zap"
)

// ErrMediaSessionNotFound is returned when a signaling or RTP-forwarding
// call names a session_id WebRTCSessionManager doesn't currently have a
// PeerConnection for (never offered, or already torn down).
var ErrMediaSessionNotFound = errors.New("relay: webrtc session not found")

// cameraStreamVideoPayloadType is an arbitrary dynamic RTP payload type
// number for the one codec this manager's PeerConnections advertise - it
// only has to be internally consistent between RegisterCodec and the track
// created from the same capability, not matched to anything else.
const cameraStreamVideoPayloadType = 102

// WebRTCSessionManager owns one ephemeral pion PeerConnection per active
// camera session - created when Google's client POSTs its SDP offer to
// cmd/googlehomed's signaling endpoint, destroyed on an explicit "end",
// ICE failure, or idle timeout. SessionManager is the token/auth layer in
// front of this; by the time any method here is called, the caller has
// already validated the session_id/token pair.
//
// Video-only, H264, for this first cut - audio is explicitly out of scope.
// The codec is pinned rather than negotiated from go2rtc dynamically,
// because the answer to Google (and the TrackLocalStaticRTP it's built
// from) has to exist before any RTP from the agent's WHEP pull arrives.
// Frigate/go2rtc's actual serving codec can't be confirmed from this repo
// (it's deployment config, not code here) - the camera/go2rtc config must
// be constrained to H264, and agent/media_session.go must report
// MediaSessionFailed rather than forward mismatched RTP if it ever isn't.
// The exact H264 fmtp/profile-level-id this advertises is also unverified
// against what Google's client actually offers - this repo has never been
// registered in the Google Actions console (see
// service/googlehome/README.md), so there's no way to test a real offer
// yet; revisit once that's possible.
type WebRTCSessionManager struct {
	logger     *zap.Logger
	sessionMgr *SessionManager

	// onTeardown is called exactly once per session, from whichever
	// trigger tears it down first. cmd/googlehomed's main.go wires this to
	// expire the SessionManager record and best-effort notify the owning
	// agent via ProviderService.StopMediaSession.
	onTeardown func(sessionID string)

	mu       sync.Mutex
	sessions map[string]*webrtcSession
}

// webrtcSession is either a placeholder claiming sessionID while
// HandleOffer is still negotiating (pc/videoTrack nil - see HandleOffer's
// doc comment) or a fully negotiated session. Every method that looks one
// up must check for the placeholder case before touching pc/videoTrack.
type webrtcSession struct {
	pc         *webrtc.PeerConnection
	videoTrack *webrtc.TrackLocalStaticRTP

	mu        sync.Mutex
	idleTimer *time.Timer
}

// NewWebRTCSessionManager creates an empty WebRTCSessionManager. sessionMgr
// is Touch-ed as RTP actually starts flowing (see WritePacket) and once an
// offer is answered (see cmd/googlehomed's camera_signaling.go), so a
// session's bearer token stays valid for as long as it's actually in use,
// not just for mediaSessionOfferTTL after Mint.
func NewWebRTCSessionManager(logger *zap.Logger, sessionMgr *SessionManager, onTeardown func(sessionID string)) *WebRTCSessionManager {
	return &WebRTCSessionManager{logger: logger, sessionMgr: sessionMgr, onTeardown: onTeardown, sessions: map[string]*webrtcSession{}}
}

// HandleOffer creates sessionID's PeerConnection, applies offerSDP, and
// returns the answer SDP once ICE candidate gathering has fully completed.
// Google's signaling protocol has no candidate-trickle action (only
// offer/answer/end), so the answer can only be sent once, with every
// candidate already included - non-trickle/vanilla ICE, via pion's
// GatheringCompletePromise, its standard idiom for exactly this.
//
// A session_id is only ever supposed to receive one offer (SessionManager
// mints a fresh one per GetCameraStream call), so a second offer for the
// same session_id is rejected outright rather than silently replacing the
// first PeerConnection in place - the first session's idle timer/ICE
// callback are keyed only by sessionID, so overwriting the map entry under
// them would let a stale callback later tear down the wrong (second,
// legitimate) PeerConnection. HandleOffer claims sessionID with a
// placeholder entry first (pc/videoTrack left nil) so a concurrent second
// offer is rejected immediately rather than racing the real negotiation
// below, and double-checks the placeholder is still there before
// publishing the real session, in case an "end" POST tore it down while
// negotiation was in flight.
func (w *WebRTCSessionManager) HandleOffer(sessionID, offerSDP string) (answerSDP string, err error) {
	w.mu.Lock()
	if _, exists := w.sessions[sessionID]; exists {
		w.mu.Unlock()
		return "", fmt.Errorf("relay: session %s already has an offer in progress", sessionID)
	}
	w.sessions[sessionID] = &webrtcSession{}
	w.mu.Unlock()

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		PayloadType:        cameraStreamVideoPayloadType,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		w.teardown(sessionID, "")
		return "", err
	}

	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		w.teardown(sessionID, "")
		return "", err
	}

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "camera-"+sessionID,
	)
	if err != nil {
		_ = pc.Close()
		w.teardown(sessionID, "")
		return "", err
	}
	if _, err := pc.AddTrack(videoTrack); err != nil {
		_ = pc.Close()
		w.teardown(sessionID, "")
		return "", err
	}

	sess := &webrtcSession{pc: pc, videoTrack: videoTrack}
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		switch state {
		case webrtc.ICEConnectionStateDisconnected, webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateClosed:
			w.teardown(sessionID, "ice "+state.String())
		}
	})

	w.mu.Lock()
	if _, stillClaimed := w.sessions[sessionID]; !stillClaimed {
		// An "end" POST (or some other teardown trigger) raced this
		// negotiation and already removed the placeholder - don't
		// resurrect a session that was just asked to end.
		w.mu.Unlock()
		_ = pc.Close()
		return "", fmt.Errorf("relay: session %s ended before negotiation completed", sessionID)
	}
	w.sessions[sessionID] = sess
	w.mu.Unlock()
	w.resetIdleTimer(sessionID, sess)

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		w.teardown(sessionID, "")
		return "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		w.teardown(sessionID, "")
		return "", err
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		w.teardown(sessionID, "")
		return "", err
	}
	<-gatherComplete

	return pc.LocalDescription().SDP, nil
}

// WritePacket forwards one RTP packet onto sessionID's outbound video
// track, resets its idle timer, and extends its SessionManager token past
// mediaSessionIdleTimeout - called by cmd/googlehomed's PushMediaSession
// RPC handler for every packet the agent forwards. Packets can arrive
// before HandleOffer ever runs (the agent starts forwarding as soon as its
// own WHEP pull succeeds, independent of when Google's client gets around
// to POSTing its offer) - ErrMediaSessionNotFound covers both that case
// and sessionID naming a not-yet-negotiated placeholder (see HandleOffer).
func (w *WebRTCSessionManager) WritePacket(sessionID string, rtp []byte) error {
	w.mu.Lock()
	sess, ok := w.sessions[sessionID]
	w.mu.Unlock()
	if !ok || sess.videoTrack == nil {
		return ErrMediaSessionNotFound
	}

	w.resetIdleTimer(sessionID, sess)
	w.sessionMgr.Touch(sessionID)
	_, err := sess.videoTrack.Write(rtp)
	return err
}

// HandleEnd tears sessionID down in response to an explicit "end" POST
// from Google's client.
func (w *WebRTCSessionManager) HandleEnd(sessionID string) {
	w.teardown(sessionID, "")
}

// TeardownSessions tears down every one of sessionIDs that's still open -
// called alongside SessionManager.SweepForAgent when an agent's relay
// stream drops, so a PeerConnection never outlives the control channel
// that could otherwise stop it.
func (w *WebRTCSessionManager) TeardownSessions(sessionIDs []string) {
	for _, id := range sessionIDs {
		w.teardown(id, "agent disconnected")
	}
}

func (w *WebRTCSessionManager) resetIdleTimer(sessionID string, sess *webrtcSession) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.idleTimer != nil {
		sess.idleTimer.Stop()
	}
	sess.idleTimer = time.AfterFunc(mediaSessionIdleTimeout, func() {
		w.teardown(sessionID, "idle timeout")
	})
}

// teardown removes sessionID from this manager and invokes onTeardown
// exactly once - safe to call more than once for the same session_id
// (e.g. an "end" POST racing an ICE failure callback): only the first call
// finds anything to do, every later one is a no-op. onTeardown always
// runs if sessionID was found, even if it still only names HandleOffer's
// placeholder (pc nil - negotiation hasn't produced a real PeerConnection
// yet): onTeardown is what expires the SessionManager token and tells the
// agent to stop pulling WHEP, neither of which HandleOffer's own
// "stillClaimed" check (which only reacts to the map entry disappearing)
// does on its own - skipping onTeardown here would leave both the token
// and the agent's WHEP pull/PushMediaSession stream running indefinitely
// for an end/failure that arrived before an offer ever did.
func (w *WebRTCSessionManager) teardown(sessionID, reason string) {
	w.mu.Lock()
	sess, ok := w.sessions[sessionID]
	if ok {
		delete(w.sessions, sessionID)
	}
	w.mu.Unlock()
	if !ok {
		return
	}

	if sess.pc != nil {
		sess.mu.Lock()
		if sess.idleTimer != nil {
			sess.idleTimer.Stop()
		}
		sess.mu.Unlock()

		if reason != "" {
			w.logger.Info("tearing down media session", zap.String("session_id", sessionID), zap.String("reason", reason))
		}
		_ = sess.pc.Close()
	}
	w.onTeardown(sessionID)
}
