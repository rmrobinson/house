package relay

import (
	"context"
	"errors"
	"sync"

	action "github.com/rmrobinson/google-smart-home-action-go"
	"go.uber.org/zap"
)

// ErrAgentNotFound is returned if Google requests an action for an agent ID
// that isn't currently registered (the agent process isn't connected).
var ErrAgentNotFound = errors.New("relay: agent not found")

// ProviderService satisfies action.Provider for whichever agent is currently
// registered. house is a single-building system, so in practice exactly one
// agent ID is ever registered at a time - this still keys by agent ID
// (rather than holding a single *RemoteProvider directly) so the relay
// protocol and googlehomed's wiring don't need to change if that ever stops
// being true.
type ProviderService struct {
	logger    *zap.Logger
	providers map[string]action.Provider

	mu sync.Mutex
}

// NewProviderService creates an empty ProviderService.
func NewProviderService(logger *zap.Logger) *ProviderService {
	return &ProviderService{
		logger:    logger,
		providers: map[string]action.Provider{},
	}
}

// RegisterProvider adds agentID to the set of currently-connected agents.
func (s *ProviderService) RegisterProvider(agentID string, provider action.Provider) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logger.Info("registering agent", zap.String("agent_id", agentID))
	s.providers[agentID] = provider
}

// UnregisterProvider removes agentID from the set of connected agents.
func (s *ProviderService) UnregisterProvider(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logger.Info("unregistering agent", zap.String("agent_id", agentID))
	delete(s.providers, agentID)
}

func (s *ProviderService) provider(agentID string) (action.Provider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.providers[agentID]
	return p, ok
}

// Sync implements action.Provider.
func (s *ProviderService) Sync(ctx context.Context, agentID string) (*action.SyncResponse, error) {
	if p, found := s.provider(agentID); found {
		return p.Sync(ctx, agentID)
	}
	return nil, ErrAgentNotFound
}

// Disconnect implements action.Provider.
func (s *ProviderService) Disconnect(ctx context.Context, agentID string) error {
	p, found := s.provider(agentID)
	if !found {
		return ErrAgentNotFound
	}

	if err := p.Disconnect(ctx, agentID); err != nil {
		s.logger.Info("error disconnecting agent", zap.String("agent_id", agentID), zap.Error(err))
	}

	s.mu.Lock()
	delete(s.providers, agentID)
	s.mu.Unlock()
	return nil
}

// Query implements action.Provider.
func (s *ProviderService) Query(ctx context.Context, req *action.QueryRequest) (*action.QueryResponse, error) {
	if p, found := s.provider(req.AgentID); found {
		return p.Query(ctx, req)
	}
	return nil, ErrAgentNotFound
}

// Execute implements action.Provider.
func (s *ProviderService) Execute(ctx context.Context, req *action.ExecuteRequest) (*action.ExecuteResponse, error) {
	if p, found := s.provider(req.AgentID); found {
		return p.Execute(ctx, req)
	}
	return nil, ErrAgentNotFound
}

// StopMediaSession best-effort asks agentID's agent to stop sessionID -
// called by WebRTCSessionManager's teardown callback (wired in
// cmd/googlehomed's main.go). agentID may no longer be registered (its
// relay stream already dropped), in which case there's nothing to notify
// and this is a no-op - that agent's own sessions were already swept via
// SessionManager.SweepForAgent when it disconnected.
func (s *ProviderService) StopMediaSession(agentID, sessionID string) {
	p, found := s.provider(agentID)
	if !found {
		return
	}
	rp, ok := p.(*RemoteProvider)
	if !ok {
		return
	}
	if err := rp.StopMediaSession(sessionID); err != nil {
		s.logger.Info("unable to stop media session", zap.String("agent_id", agentID), zap.String("session_id", sessionID), zap.Error(err))
	}
}

var _ action.Provider = (*ProviderService)(nil)
