package relay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSessionManager_MintAndValidate(t *testing.T) {
	m := NewSessionManager()
	sessionID, token := m.Mint("agent1", "device1")

	deviceID, ok := m.Validate(sessionID, token)
	assert.True(t, ok)
	assert.Equal(t, "device1", deviceID)
}

func TestSessionManager_ValidateWrongToken(t *testing.T) {
	m := NewSessionManager()
	sessionID, _ := m.Mint("agent1", "device1")

	_, ok := m.Validate(sessionID, "wrong-token")
	assert.False(t, ok)
}

func TestSessionManager_ValidateUnknownSession(t *testing.T) {
	m := NewSessionManager()
	_, ok := m.Validate("no-such-session", "any-token")
	assert.False(t, ok)
}

func TestSessionManager_ValidateExpired(t *testing.T) {
	m := NewSessionManager()
	sessionID, token := m.Mint("agent1", "device1")

	m.mu.Lock()
	m.sessions[sessionID].expiry = time.Now().Add(-time.Second)
	m.mu.Unlock()

	_, ok := m.Validate(sessionID, token)
	assert.False(t, ok)
}

func TestSessionManager_Touch(t *testing.T) {
	m := NewSessionManager()
	sessionID, token := m.Mint("agent1", "device1")

	m.mu.Lock()
	m.sessions[sessionID].expiry = time.Now().Add(-time.Second)
	m.mu.Unlock()

	m.Touch(sessionID)

	_, ok := m.Validate(sessionID, token)
	assert.True(t, ok)
}

func TestSessionManager_Expire(t *testing.T) {
	m := NewSessionManager()
	sessionID, token := m.Mint("agent1", "device1")

	agentID, ok := m.Expire(sessionID)
	assert.True(t, ok)
	assert.Equal(t, "agent1", agentID)

	_, ok = m.Validate(sessionID, token)
	assert.False(t, ok)

	_, ok = m.Expire(sessionID)
	assert.False(t, ok, "expiring an already-gone session should report ok=false, not panic or double-return")
}

func TestSessionManager_SweepForAgent(t *testing.T) {
	m := NewSessionManager()
	session1, _ := m.Mint("agent1", "device1")
	session2, _ := m.Mint("agent1", "device2")
	otherAgentSession, token3 := m.Mint("agent2", "device3")

	swept := m.SweepForAgent("agent1")
	assert.ElementsMatch(t, []string{session1, session2}, swept)

	// agent2's session must survive agent1's sweep untouched.
	deviceID, ok := m.Validate(otherAgentSession, token3)
	assert.True(t, ok)
	assert.Equal(t, "device3", deviceID)
}

func TestSessionManager_MintSweepsExpiredEntries(t *testing.T) {
	m := NewSessionManager()
	staleID, _ := m.Mint("agent1", "device1")
	m.mu.Lock()
	m.sessions[staleID].expiry = time.Now().Add(-time.Minute)
	m.mu.Unlock()

	m.Mint("agent1", "device2")

	m.mu.Lock()
	_, stillThere := m.sessions[staleID]
	m.mu.Unlock()
	assert.False(t, stillThere, "Mint should opportunistically reap already-expired sessions")
}

// TestSessionManager_SweepExpiredNotifiesOwner covers the real bug this
// method exists to fix: a session whose offer never arrives at all (not
// one that failed/disconnected after signaling) was never reaped by
// anything that actually notified the owning agent - only by Mint's own
// silent, best-effort sweep, which doesn't run at all unless a new
// session happens to be minted. The agent would otherwise keep pulling
// WHEP/pushing RTP for it indefinitely.
func TestSessionManager_SweepExpiredNotifiesOwner(t *testing.T) {
	m := NewSessionManager()
	staleID, _ := m.Mint("agent1", "device1")
	freshID, _ := m.Mint("agent1", "device2")
	m.mu.Lock()
	m.sessions[staleID].expiry = time.Now().Add(-time.Minute)
	m.mu.Unlock()

	expired := m.SweepExpired()
	assert.Equal(t, []ExpiredSession{{AgentID: "agent1", SessionID: staleID}}, expired)

	m.mu.Lock()
	_, staleStillThere := m.sessions[staleID]
	_, freshStillThere := m.sessions[freshID]
	m.mu.Unlock()
	assert.False(t, staleStillThere)
	assert.True(t, freshStillThere, "SweepExpired must not touch a session that hasn't expired yet")

	// A second sweep with nothing newly expired reports no one to notify.
	assert.Empty(t, m.SweepExpired())
}
