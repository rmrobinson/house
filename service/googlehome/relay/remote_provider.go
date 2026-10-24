package relay

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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
	// sendMu serializes every stream.Send call: grpc-go's ServerStream.Send
	// is documented as unsafe to call concurrently, but this one stream can
	// be driven by independent goroutines at once - concurrent fulfillment
	// HTTP requests (e.g. Google issuing SYNC and EXECUTE close together)
	// each reach this same RemoteProvider and call doRequest/Disconnect on
	// their own goroutine.
	sendMu sync.Mutex

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
//
// It looks up *and* removes the outstanding entry under one lock so a
// request ID can only ever be completed once - either here, or by
// doRequest's own timeout/cleanup finding nothing left to do - rather than
// both racing over the same channel. It then sends with the lock already
// released, onto a channel buffered to 1, so this never blocks even if
// doRequest gave up waiting (ctx done or timed out) before this response
// arrived: holding mu while sending to an unbuffered channel used to let a
// late response and a timing-out doRequest deadlock each other, which also
// wedged this whole method - and since it runs synchronously from
// API.StateSync's single Recv loop, that stalled every other message from
// this agent too.
func (m *RemoteProvider) receiveResponse(commandResp *ClientRequest) error {
	m.mu.Lock()
	ch, ok := m.outstandingReqs[commandResp.RequestId]
	if ok {
		delete(m.outstandingReqs, commandResp.RequestId)
	}
	m.mu.Unlock()

	if !ok {
		m.logger.Info("received response for request id no longer pending",
			zap.String("agent_id", m.agentID),
			zap.String("request_id", commandResp.RequestId),
		)
		return ErrRequestMissing
	}

	ch <- commandResp
	return nil
}

func (m *RemoteProvider) doRequest(ctx context.Context, serverReq *ServerRequest) (*ClientRequest, error) {
	reqID := uuid.New().String()
	serverReq.RequestId = reqID

	// Buffered to 1: see receiveResponse's doc comment for why a send must
	// never block here.
	respCh := make(chan *ClientRequest, 1)
	m.mu.Lock()
	m.outstandingReqs[reqID] = respCh
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.outstandingReqs, reqID)
		m.mu.Unlock()
	}()

	m.sendMu.Lock()
	err := m.stream.Send(serverReq)
	m.sendMu.Unlock()
	if err != nil {
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
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
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

	// action.ExecuteResponse (google-smart-home-action-go's device.go) can
	// only carry one shared UpdatedState for every UpdatedDevices entry - it
	// has no per-device/per-group slot, a limitation the nerves precedent
	// this package is modeled on hit too (see its proxy.go's own "NOTE: not
	// really sure what to do at this point" comment on the identical
	// problem). Since our agent reports one CommandResult per device (see
	// agent.go's executeOne), a batch EXECUTE against several devices can
	// legitimately come back with more than one distinct resulting state
	// (e.g. the same OnOff command applied to two lights that started at
	// different brightness levels). Rather than just keeping whichever
	// result happened to be last - silently misreporting every other
	// device's state - group identical states together and keep the
	// largest group: every successful device is still reported as updated
	// (it did succeed; reporting it OFFLINE/ERROR instead would be worse),
	// just with that group's state where it differs from its own actual
	// one, until the next ReportState/QUERY corrects it.
	type stateGroup struct {
		deviceIDs []string
		state     action.DeviceState
	}
	var successGroups []stateGroup

	for _, result := range execResp.ExecuteResponse.Results {
		switch result.Status {
		case ExecuteResponse_SUCCESS:
			var state action.DeviceState
			if err := json.Unmarshal(result.States, &state); err != nil {
				return nil, ErrUnsupportedCommandResponseReceived
			}

			merged := false
			for i := range successGroups {
				if reflect.DeepEqual(successGroups[i].state, state) {
					successGroups[i].deviceIDs = append(successGroups[i].deviceIDs, result.DeviceIds...)
					merged = true
					break
				}
			}
			if !merged {
				successGroups = append(successGroups, stateGroup{
					deviceIDs: append([]string{}, result.DeviceIds...),
					state:     state,
				})
			}
		case ExecuteResponse_OFFLINE:
			actionResp.OfflineDevices = append(actionResp.OfflineDevices, result.DeviceIds...)
		case ExecuteResponse_ERROR:
			actionResp.FailedDevices[result.ErrorCode] = struct{ Devices []string }{Devices: result.DeviceIds}
		}
	}

	if len(successGroups) > 0 {
		best := successGroups[0]
		for _, g := range successGroups[1:] {
			if len(g.deviceIDs) > len(best.deviceIDs) {
				best = g
			}
		}
		actionResp.UpdatedState = best.state
		for _, g := range successGroups {
			actionResp.UpdatedDevices = append(actionResp.UpdatedDevices, g.deviceIDs...)
		}
	}

	return actionResp, nil
}

var _ action.Provider = (*RemoteProvider)(nil)
