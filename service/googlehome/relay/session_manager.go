package relay

import (
	"crypto/subtle"
	"sync"
	"time"

	"github.com/google/uuid"
)

// CameraSignalingPath is the HTTP path cmd/googlehomed serves the camera
// signaling endpoint on, and the path RemoteProvider.getCameraStream
// builds cameraStreamSignalingUrl from - kept here since both sides of
// that agreement need it and this package is the shared one.
const CameraSignalingPath = "/camera-stream"

const (
	// mediaSessionOfferTTL bounds how long a freshly minted session's token
	// is valid before Google's client must POST its SDP offer to the
	// signaling endpoint - Google's docs give no guidance here, so this is
	// a deliberately generous guess (the client posts the offer almost
	// immediately after EXECUTE succeeds in practice).
	mediaSessionOfferTTL = 30 * time.Second
	// mediaSessionIdleTimeout tears a connected session down if no RTP has
	// arrived for this long - same no-guidance caveat as above.
	mediaSessionIdleTimeout = 60 * time.Second
)

// session is one in-flight GetCameraStream request: minted on EXECUTE,
// validated on every signaling POST, torn down on end/ICE failure/idle
// timeout/agent failure/the owning agent's relay stream dropping.
type session struct {
	deviceID string
	agentID  string
	token    string
	expiry   time.Time
}

// SessionManager mints and validates the per-session bearer tokens that
// gate cmd/googlehomed's camera signaling endpoint - see
// WebRTCSessionManager for what actually happens once a session is
// validated. Deliberately lives in this package rather than a sibling one:
// it's small, and only RemoteProvider.Execute (mint) and the signaling
// HTTP handler (validate) need it.
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*session
}

// NewSessionManager creates an empty SessionManager.
func NewSessionManager() *SessionManager {
	return &SessionManager{sessions: map[string]*session{}}
}

// Mint creates a new session for deviceID, owned by agentID (so a later
// SweepForAgent can find it if that agent's relay stream drops), and
// returns its session_id and bearer token. Opportunistically sweeps
// already-expired sessions first (ignoring the result - see SweepExpired's
// doc comment for why that's fine here specifically).
func (m *SessionManager) Mint(agentID, deviceID string) (sessionID, token string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sweepExpiredLocked()

	sessionID = uuid.New().String()
	token = uuid.New().String()
	m.sessions[sessionID] = &session{
		deviceID: deviceID,
		agentID:  agentID,
		token:    token,
		expiry:   time.Now().Add(mediaSessionOfferTTL),
	}
	return sessionID, token
}

// Validate checks sessionID/token against what Mint recorded and that it
// hasn't expired, returning the device it's scoped to.
func (m *SessionManager) Validate(sessionID, token string) (deviceID string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, found := m.sessions[sessionID]
	if !found || time.Now().After(s.expiry) {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
		return "", false
	}
	return s.deviceID, true
}

// Touch extends sessionID's expiry by mediaSessionIdleTimeout - called
// every time RTP actually arrives for a connected session, so an active
// stream isn't torn down out from under the viewer.
func (m *SessionManager) Touch(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[sessionID]; ok {
		s.expiry = time.Now().Add(mediaSessionIdleTimeout)
	}
}

// Expire unconditionally removes sessionID, returning the agent_id it
// belonged to (so the caller can best-effort notify that agent to stop),
// or ok=false if it was already gone.
func (m *SessionManager) Expire(sessionID string) (agentID string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, found := m.sessions[sessionID]
	if !found {
		return "", false
	}
	delete(m.sessions, sessionID)
	return s.agentID, true
}

// ExpiredSession identifies one session SweepExpired just removed, for the
// caller to notify.
type ExpiredSession struct {
	AgentID   string
	SessionID string
}

// sweepExpiredLocked removes every session past its expiry, returning who
// to notify. m.mu must already be held by the caller.
func (m *SessionManager) sweepExpiredLocked() []ExpiredSession {
	var expired []ExpiredSession
	now := time.Now()
	for id, s := range m.sessions {
		if now.After(s.expiry) {
			expired = append(expired, ExpiredSession{AgentID: s.agentID, SessionID: id})
			delete(m.sessions, id)
		}
	}
	return expired
}

// SweepExpired removes every session past its expiry and returns who to
// notify - meant to be called periodically (see main.go's sweep loop).
// Mint's own opportunistic sweep uses the same underlying logic but
// discards the result: it only runs when a new camera session happens to
// be minted, which could be arbitrarily rare, and silently dropping the
// bookkeeping there without notifying the agent would just leave it
// pulling WHEP/pushing RTP forever for a session nobody will ever claim -
// exactly the bug this periodic sweep exists to avoid for the common
// case (Google's client never POSTs an offer at all).
func (m *SessionManager) SweepExpired() []ExpiredSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sweepExpiredLocked()
}

// SweepForAgent removes and returns the session_ids owned by agentID -
// called when that agent's relay stream drops, since no further
// StopMediaSession can be delivered to it anyway.
func (m *SessionManager) SweepForAgent(agentID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var ids []string
	for id, s := range m.sessions {
		if s.agentID == agentID {
			ids = append(ids, id)
			delete(m.sessions, id)
		}
	}
	return ids
}
