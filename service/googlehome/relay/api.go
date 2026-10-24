// Package relay implements the googlehomed side of the agent<->relay
// protocol (googlehome.proto): API is the RelayService gRPC server the
// agent dials into, ProviderService satisfies action.Provider by looking up
// whichever agent is currently registered, and RemoteProvider is the
// per-agent action.Provider that actually relays SYNC/QUERY/EXECUTE calls
// over that agent's stream and waits for its response.
package relay

import (
	"encoding/json"
	"fmt"
	"io"

	action "github.com/rmrobinson/google-smart-home-action-go"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrNoAgentRegistered is returned when a message is received on a stream
// that hasn't yet sent a RegisterAgentRequest.
var ErrNoAgentRegistered = status.New(codes.InvalidArgument, "no agent registered")

// API implements RelayServiceServer.
type API struct {
	UnimplementedRelayServiceServer

	logger *zap.Logger

	providerSvc *ProviderService
	actionSvc   *action.Service
}

// NewAPI creates an API backed by ps (looked up by action.Service to answer
// SYNC/QUERY/EXECUTE) and as (used to push RequestSync/ReportState calls on
// to HomeGraph when the agent asks for them).
func NewAPI(logger *zap.Logger, ps *ProviderService, as *action.Service) *API {
	return &API{logger: logger, providerSvc: ps, actionSvc: as}
}

// StateSync implements RelayServiceServer: it must receive a RegisterAgent
// message first, then relays every subsequent message either to googlehomed
// itself (RequestSync, ReportState) or to the registered RemoteProvider as a
// response to one of its outstanding requests.
func (a *API) StateSync(stream RelayService_StateSyncServer) error {
	var provider *RemoteProvider
	for {
		reqMsg, err := stream.Recv()
		if err == io.EOF {
			if provider != nil {
				a.providerSvc.UnregisterProvider(provider.agentID)
			}
			return nil
		}
		if err != nil {
			a.logger.Error("error receiving from stream", zap.Error(err))
			return err
		}

		if req := reqMsg.GetRegisterAgent(); req != nil {
			provider = NewRemoteProvider(a.logger, req.AgentId, stream)
			a.providerSvc.RegisterProvider(req.AgentId, provider)
			continue
		}

		if provider == nil {
			a.logger.Info("received message before agent registered, closing stream")
			return ErrNoAgentRegistered.Err()
		}

		switch {
		case reqMsg.GetRequestSync() != nil:
			// RequestSync calls HomeGraph synchronously, and the result of
			// that call has nothing to do with this stream - run it on its
			// own goroutine so a slow/failing HomeGraph call never stalls
			// the agent's StateSync loop.
			go func() {
				if err := a.actionSvc.RequestSync(stream.Context(), provider.agentID); err != nil {
					a.logger.Error("unable to request sync", zap.String("agent_id", provider.agentID), zap.Error(err))
				}
			}()
		case reqMsg.GetReportState() != nil:
			var deviceStates map[string]action.DeviceState
			if err := json.Unmarshal(reqMsg.GetReportState().GetPayload(), &deviceStates); err != nil {
				a.logger.Error("unable to deserialize reported device state", zap.String("agent_id", provider.agentID), zap.Error(err))
				continue
			}
			if err := a.actionSvc.ReportState(stream.Context(), provider.agentID, deviceStates); err != nil {
				a.logger.Error("unable to report state to homegraph", zap.String("agent_id", provider.agentID), zap.Error(err))
			}
		case reqMsg.GetField() != nil:
			// Anything left with a field set is a response to one of
			// RemoteProvider's outstanding requests.
			if err := provider.receiveResponse(reqMsg); err != nil {
				a.logger.Info("unable to process response", zap.String("agent_id", provider.agentID), zap.Error(err))
			}
		default:
			a.logger.Info("received unhandled message type", zap.String("agent_id", provider.agentID), zap.String("type", fmt.Sprintf("%T", reqMsg.GetField())))
		}
	}
}
