package relay

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	action "github.com/rmrobinson/google-smart-home-action-go"
	"go.uber.org/zap"
)

// requestTimeout bounds how long RemoteProvider waits for the agent to
// answer a SYNC/QUERY/EXECUTE request sent over the stream before giving up -
// the agent itself is expected to resolve (or time out) its own
// BridgeService.ExecuteCommand call well under this.
const requestTimeout = 15 * time.Second

var (
	// ErrUnsupportedCommandResponseReceived is returned if the matched-up
	// response isn't the expected response type.
	ErrUnsupportedCommandResponseReceived = errors.New("relay: unsupported command response received")
	// ErrCommandTimeout is returned if the agent doesn't respond in time.
	ErrCommandTimeout = errors.New("relay: command request timed out")
	// ErrRequestMissing is returned if a response arrives for a request ID
	// that is no longer pending (already timed out, or never sent).
	ErrRequestMissing = errors.New("relay: request missing")
	// ErrContextTimeout is returned if the caller's own context ends first.
	ErrContextTimeout = errors.New("relay: context timed out")
	// ErrCommandFailed is returned if the agent reported an error processing
	// the request.
	ErrCommandFailed = errors.New("relay: command failed")
)

// RemoteProvider implements action.Provider by relaying every call over one
// agent's StateSync stream and waiting for the matching response.
type RemoteProvider struct {
	logger  *zap.Logger
	agentID string

	stream RelayService_StateSyncServer

	mu              sync.Mutex
	outstandingReqs map[string]chan *ClientRequest
}

// NewRemoteProvider creates a RemoteProvider backed by stream, which must
// have already received a RegisterAgentRequest for agentID.
func NewRemoteProvider(logger *zap.Logger, agentID string, stream RelayService_StateSyncServer) *RemoteProvider {
	return &RemoteProvider{
		logger:          logger,
		agentID:         agentID,
		stream:          stream,
		outstandingReqs: map[string]chan *ClientRequest{},
	}
}

// receiveResponse hands commandResp to whichever doRequest call is waiting
// on its request ID. Called from API.StateSync's receive loop for every
// ClientRequest that isn't one of the control messages it handles directly.
func (m *RemoteProvider) receiveResponse(commandResp *ClientRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ch, ok := m.outstandingReqs[commandResp.RequestId]; ok {
		ch <- commandResp
		return nil
	}

	m.logger.Info("received response for request id no longer pending",
		zap.String("agent_id", m.agentID),
		zap.String("request_id", commandResp.RequestId),
	)
	return ErrRequestMissing
}

func (m *RemoteProvider) doRequest(ctx context.Context, serverReq *ServerRequest) (*ClientRequest, error) {
	reqID := uuid.New().String()
	serverReq.RequestId = reqID

	respCh := make(chan *ClientRequest)
	m.mu.Lock()
	m.outstandingReqs[reqID] = respCh
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.outstandingReqs, reqID)
		close(respCh)
		m.mu.Unlock()
	}()

	if err := m.stream.Send(serverReq); err != nil {
		m.logger.Error("failed to send request",
			zap.String("agent_id", m.agentID),
			zap.String("request_id", reqID),
			zap.Error(err),
		)
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ErrContextTimeout
	case resp := <-respCh:
		if resp.GetRequestId() != reqID {
			m.logger.Error("mismatched request id", zap.String("agent_id", m.agentID))
		} else if resp.GetField() == nil {
			return nil, ErrUnsupportedCommandResponseReceived
		}
		return resp, nil
	case <-time.After(requestTimeout):
		return nil, ErrCommandTimeout
	}
}

// Sync implements action.Provider.
func (m *RemoteProvider) Sync(ctx context.Context, _ string) (*action.SyncResponse, error) {
	resp, err := m.doRequest(ctx, &ServerRequest{Field: &ServerRequest_SyncRequest{SyncRequest: &SyncRequest{}}})
	if err != nil {
		return nil, err
	}

	syncResp, ok := resp.GetField().(*ClientRequest_SyncResponse)
	if !ok {
		return nil, ErrUnsupportedCommandResponseReceived
	}
	if len(syncResp.SyncResponse.ErrorDetails) > 0 {
		m.logger.Info("agent reported error processing sync",
			zap.String("agent_id", m.agentID),
			zap.String("error_details", syncResp.SyncResponse.ErrorDetails),
		)
		return nil, ErrCommandFailed
	}

	actionResp := &action.SyncResponse{}
	if err := json.Unmarshal(syncResp.SyncResponse.Payload, &actionResp.Devices); err != nil {
		return nil, ErrUnsupportedCommandResponseReceived
	}
	return actionResp, nil
}

// Disconnect implements action.Provider.
func (m *RemoteProvider) Disconnect(context.Context, string) error {
	return m.stream.Send(&ServerRequest{Field: &ServerRequest_DisconnectRequest{DisconnectRequest: &DisconnectRequest{}}})
}

// Query implements action.Provider.
func (m *RemoteProvider) Query(ctx context.Context, req *action.QueryRequest) (*action.QueryResponse, error) {
	var deviceIDs []string
	for _, d := range req.Devices {
		deviceIDs = append(deviceIDs, d.ID)
	}

	resp, err := m.doRequest(ctx, &ServerRequest{
		Field: &ServerRequest_QueryRequest{QueryRequest: &QueryRequest{DeviceIds: deviceIDs}},
	})
	if err != nil {
		return nil, err
	}

	queryResp, ok := resp.GetField().(*ClientRequest_QueryResponse)
	if !ok {
		return nil, ErrUnsupportedCommandResponseReceived
	}
	if len(queryResp.QueryResponse.ErrorDetails) > 0 {
		m.logger.Info("agent reported error processing query",
			zap.String("agent_id", m.agentID),
			zap.String("error_details", queryResp.QueryResponse.ErrorDetails),
		)
		return nil, ErrCommandFailed
	}

	actionResp := &action.QueryResponse{States: map[string]action.DeviceState{}}
	if err := json.Unmarshal(queryResp.QueryResponse.Payload, &actionResp.States); err != nil {
		return nil, ErrUnsupportedCommandResponseReceived
	}
	return actionResp, nil
}

// Execute implements action.Provider.
func (m *RemoteProvider) Execute(ctx context.Context, req *action.ExecuteRequest) (*action.ExecuteResponse, error) {
	var execCommands []*ExecuteRequest_Command
	for _, commandArg := range req.Commands {
		execCommand := &ExecuteRequest_Command{}
		for _, deviceArg := range commandArg.TargetDevices {
			execCommand.DeviceIds = append(execCommand.DeviceIds, deviceArg.ID)
		}
		for _, cmd := range commandArg.Commands {
			payload, err := json.Marshal(cmd)
			if err != nil {
				return nil, err
			}
			execCommand.ExecutionContext = append(execCommand.ExecutionContext, &ExecuteRequest_Command_ExecutionContext{Payload: payload})
		}
		execCommands = append(execCommands, execCommand)
	}

	resp, err := m.doRequest(ctx, &ServerRequest{
		Field: &ServerRequest_ExecuteRequest{ExecuteRequest: &ExecuteRequest{Commands: execCommands}},
	})
	if err != nil {
		return nil, err
	}

	execResp, ok := resp.GetField().(*ClientRequest_ExecuteResponse)
	if !ok {
		return nil, ErrUnsupportedCommandResponseReceived
	}
	if len(execResp.ExecuteResponse.ErrorDetails) > 0 {
		m.logger.Info("agent reported error processing execute",
			zap.String("agent_id", m.agentID),
			zap.String("error_details", execResp.ExecuteResponse.ErrorDetails),
		)
		return nil, ErrCommandFailed
	}

	actionResp := &action.ExecuteResponse{
		FailedDevices: map[string]struct{ Devices []string }{},
	}
	for _, result := range execResp.ExecuteResponse.Results {
		switch result.Status {
		case ExecuteResponse_SUCCESS:
			if err := json.Unmarshal(result.States, &actionResp.UpdatedState); err != nil {
				return nil, ErrUnsupportedCommandResponseReceived
			}
			actionResp.UpdatedDevices = append(actionResp.UpdatedDevices, result.DeviceIds...)
		case ExecuteResponse_OFFLINE:
			actionResp.OfflineDevices = append(actionResp.OfflineDevices, result.DeviceIds...)
		case ExecuteResponse_ERROR:
			actionResp.FailedDevices[result.ErrorCode] = struct{ Devices []string }{Devices: result.DeviceIds}
		}
	}

	return actionResp, nil
}

var _ action.Provider = (*RemoteProvider)(nil)
