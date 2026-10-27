package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/google/uuid"
	action "github.com/rmrobinson/google-smart-home-action-go"
	"go.uber.org/zap"
)

// getCameraStreamCommandName is the one EXECUTE command this package
// intercepts before it ever reaches the agent's generic BridgeService
// command path - see Execute's doc comment for why.
const getCameraStreamCommandName = "action.devices.commands.GetCameraStream"

// requestTimeout bounds how long RemoteProvider waits for the agent to
// answer a SYNC/QUERY/EXECUTE request sent over the stream before giving up -
// the agent itself is expected to resolve (or time out) its own
// BridgeService.ExecuteCommand call well under this.
const requestTimeout = 15 * time.Second

// mediaSessionStartTimeout bounds how long RemoteProvider waits for the
// agent's StartMediaSession reply specifically - longer than
// requestTimeout because a camera session start is a real network round
// trip (BridgeService.GetDevice, then a WHEP offer/answer against go2rtc,
// including its own ICE-gathering wait, bounded by
// agent/media_session.go's whepTrackTimeout) rather than a local
// BridgeService.ExecuteCommand call. This house has previously hit go2rtc
// WHEP startup stalling past 10s on unreachable STUN (see the
// go2rtc_stun_slow_whep incident) - 15s would leave almost no margin on
// top of that.
const mediaSessionStartTimeout = 30 * time.Second

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

	stream        RelayService_StateSyncServer
	sessionMgr    *SessionManager
	publicBaseURL string
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
// have already received a RegisterAgentRequest for agentID. sessionMgr and
// publicBaseURL back GetCameraStream handling (see Execute) -
// cmd/googlehomed's main.go constructs one SessionManager shared by every
// RemoteProvider.
func NewRemoteProvider(logger *zap.Logger, agentID string, stream RelayService_StateSyncServer, sessionMgr *SessionManager, publicBaseURL string) *RemoteProvider {
	return &RemoteProvider{
		logger:          logger,
		agentID:         agentID,
		stream:          stream,
		sessionMgr:      sessionMgr,
		publicBaseURL:   publicBaseURL,
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

func (m *RemoteProvider) doRequest(ctx context.Context, serverReq *ServerRequest, timeout time.Duration) (*ClientRequest, error) {
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
	case <-time.After(timeout):
		return nil, ErrCommandTimeout
	}
}

// Sync implements action.Provider.
func (m *RemoteProvider) Sync(ctx context.Context, _ string) (*action.SyncResponse, error) {
	resp, err := m.doRequest(ctx, &ServerRequest{Field: &ServerRequest_SyncRequest{SyncRequest: &SyncRequest{}}}, requestTimeout)
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

// StartMediaSession asks the agent to start pulling deviceID's local WHEP
// stream and forwarding it (via the separate PushMediaSession RPC) under
// sessionID, waiting for the agent's MediaSessionReady/MediaSessionFailed
// reply - reuses doRequest exactly as Sync/Query/Execute do, since it's
// already generic over whichever ServerRequest/ClientRequest oneof case is
// in play.
func (m *RemoteProvider) StartMediaSession(ctx context.Context, sessionID, deviceID string) error {
	resp, err := m.doRequest(ctx, &ServerRequest{
		Field: &ServerRequest_StartMediaSession{StartMediaSession: &StartMediaSession{SessionId: sessionID, DeviceId: deviceID}},
	}, mediaSessionStartTimeout)
	if err != nil {
		return err
	}

	switch r := resp.GetField().(type) {
	case *ClientRequest_MediaSessionReady:
		return nil
	case *ClientRequest_MediaSessionFailed:
		return fmt.Errorf("relay: agent failed to start media session: %s", r.MediaSessionFailed.GetError())
	default:
		return ErrUnsupportedCommandResponseReceived
	}
}

// StopMediaSession asks the agent to stop sessionID - best-effort,
// fire-and-forget like Disconnect: every caller (explicit end, ICE
// failure, idle timeout, this agent's own relay stream dropping) is
// already tearing the session down on the googlehomed side regardless of
// whether this send succeeds.
func (m *RemoteProvider) StopMediaSession(sessionID string) error {
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	return m.stream.Send(&ServerRequest{
		Field: &ServerRequest_StopMediaSession{StopMediaSession: &StopMediaSession{SessionId: sessionID}},
	})
}

// Query implements action.Provider.
func (m *RemoteProvider) Query(ctx context.Context, req *action.QueryRequest) (*action.QueryResponse, error) {
	var deviceIDs []string
	for _, d := range req.Devices {
		deviceIDs = append(deviceIDs, d.ID)
	}

	resp, err := m.doRequest(ctx, &ServerRequest{
		Field: &ServerRequest_QueryRequest{QueryRequest: &QueryRequest{DeviceIds: deviceIDs}},
	}, requestTimeout)
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

// getCameraStreamCommand returns the GetCameraStream command's params in
// cmds, if present - Google never batches this with other commands in
// practice (it's always the sole command in its own EXECUTE intent), so
// the first match is treated as authoritative and anything else in the
// same group is ignored.
func getCameraStreamCommand(cmds []action.Command) (params map[string]interface{}, ok bool) {
	for _, c := range cmds {
		if c.Generic != nil && c.Generic.Command == getCameraStreamCommandName {
			return c.Generic.Params, true
		}
	}
	return nil, false
}

// paramsSupportWebRTC reports whether a GetCameraStream request's
// SupportedStreamProtocols param includes "webrtc" - params is whatever
// JSON came in, so values arrive as []interface{} of strings even though
// Google's docs describe the param as a plain string array.
func paramsSupportWebRTC(params map[string]interface{}) bool {
	protos, _ := params["SupportedStreamProtocols"].([]interface{})
	for _, p := range protos {
		if s, ok := p.(string); ok && s == "webrtc" {
			return true
		}
	}
	return false
}

// getCameraStream handles one GetCameraStream request for deviceID: mints
// a session, asks the agent (over the existing control stream, via
// StartMediaSession) to start pulling that device's local WHEP endpoint
// and forwarding it, and returns the DeviceState Google expects in the
// EXECUTE response - see session_manager.go/webrtc_session.go for the
// session this kicks off. ok=false (the caller reports the device
// OFFLINE) covers both "doesn't support webrtc" and "the agent couldn't
// start the session" - from Google's perspective an unreachable camera
// stream looks the same as an unreachable device either way.
func (m *RemoteProvider) getCameraStream(ctx context.Context, deviceID string, params map[string]interface{}) (action.DeviceState, bool) {
	if !paramsSupportWebRTC(params) {
		m.logger.Info("GetCameraStream request didn't list webrtc as a supported protocol", zap.String("device_id", deviceID))
		return action.DeviceState{}, false
	}

	sessionID, token := m.sessionMgr.Mint(m.agentID, deviceID)

	if err := m.StartMediaSession(ctx, sessionID, deviceID); err != nil {
		m.logger.Info("media session failed to start",
			zap.String("agent_id", m.agentID), zap.String("device_id", deviceID), zap.Error(err))
		m.sessionMgr.Expire(sessionID)
		return action.DeviceState{}, false
	}

	state := action.NewDeviceState(true)
	state.State["cameraStreamProtocol"] = "webrtc"
	state.State["cameraStreamSignalingUrl"] = fmt.Sprintf("%s%s?session=%s", m.publicBaseURL, CameraSignalingPath, sessionID)
	state.State["cameraStreamAuthToken"] = token
	return state, true
}

// Execute implements action.Provider. GetCameraStream is intercepted here,
// before anything is sent to the agent's generic per-device command path:
// it has no BridgeService command to issue (there's no device state to
// mutate) and instead needs googlehomed itself to mint a session and drive
// a StartMediaSession/MediaSessionReady round-trip (see getCameraStream).
// Every other command flows through to the agent exactly as before.
func (m *RemoteProvider) Execute(ctx context.Context, req *action.ExecuteRequest) (*action.ExecuteResponse, error) {
	actionResp := &action.ExecuteResponse{
		FailedDevices: map[string]struct{ Devices []string }{},
	}

	// action.ExecuteResponse (google-smart-home-action-go's device.go) can
	// only carry one shared UpdatedState for every UpdatedDevices entry - it
	// has no per-device/per-group slot, a limitation the nerves precedent
	// this package is modeled on hit too (see its proxy.go's own "NOTE: not
	// really sure what to do at this point" comment on the identical
	// problem). Since our agent reports one CommandResult per device (see
	// agent.go's executeOne) - and a camera session's state is unique per
	// device by construction (distinct session_id/token) - a batch EXECUTE
	// against several devices can legitimately come back with more than
	// one distinct resulting state. Rather than just keeping whichever
	// result happened to be last - silently misreporting every other
	// device's state - group identical states together and keep the
	// largest group: every successful device is still reported as updated
	// (it did succeed; reporting it OFFLINE/ERROR instead would be worse),
	// just with that group's state where it differs from its own actual
	// one, until the next ReportState/QUERY corrects it.
	//
	// Camera groups are the one exception to "report every successful
	// device under the winning group's state anyway": a losing group's
	// device getting the winner's state is a harmless, self-correcting
	// cosmetic mismatch for an ordinary device (brightness, say), but for
	// a GetCameraStream device it would mean reporting either a foreign
	// cameraStreamAuthToken as this device's result, or this camera's own
	// unique token as some unrelated device's result - a real session
	// token, not cosmetic. isCamera marks groups that must never be
	// unioned into a win they didn't have: a camera-stream device is only
	// ever included in UpdatedDevices when its own group is the winner,
	// never riding along under a foreign one (or vice versa).
	type stateGroup struct {
		deviceIDs []string
		state     action.DeviceState
		isCamera  bool
	}
	var successGroups []stateGroup
	addSuccess := func(deviceIDs []string, state action.DeviceState, isCamera bool) {
		for i := range successGroups {
			if successGroups[i].isCamera == isCamera && reflect.DeepEqual(successGroups[i].state, state) {
				successGroups[i].deviceIDs = append(successGroups[i].deviceIDs, deviceIDs...)
				return
			}
		}
		successGroups = append(successGroups, stateGroup{deviceIDs: append([]string{}, deviceIDs...), state: state, isCamera: isCamera})
	}

	var relayCommands []*ExecuteRequest_Command
	for _, commandArg := range req.Commands {
		if params, ok := getCameraStreamCommand(commandArg.Commands); ok {
			for _, deviceArg := range commandArg.TargetDevices {
				state, ok := m.getCameraStream(ctx, deviceArg.ID, params)
				if !ok {
					actionResp.OfflineDevices = append(actionResp.OfflineDevices, deviceArg.ID)
					continue
				}
				addSuccess([]string{deviceArg.ID}, state, true)
			}
			continue
		}

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
		relayCommands = append(relayCommands, execCommand)
	}

	if len(relayCommands) > 0 {
		resp, err := m.doRequest(ctx, &ServerRequest{
			Field: &ServerRequest_ExecuteRequest{ExecuteRequest: &ExecuteRequest{Commands: relayCommands}},
		}, requestTimeout)
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

		for _, result := range execResp.ExecuteResponse.Results {
			switch result.Status {
			case ExecuteResponse_SUCCESS:
				var state action.DeviceState
				if err := json.Unmarshal(result.States, &state); err != nil {
					return nil, ErrUnsupportedCommandResponseReceived
				}
				addSuccess(result.DeviceIds, state, false)
			case ExecuteResponse_OFFLINE:
				actionResp.OfflineDevices = append(actionResp.OfflineDevices, result.DeviceIds...)
			case ExecuteResponse_ERROR:
				actionResp.FailedDevices[result.ErrorCode] = struct{ Devices []string }{Devices: result.DeviceIds}
			}
		}
	}

	if len(successGroups) > 0 {
		bestIdx := 0
		for i := 1; i < len(successGroups); i++ {
			if len(successGroups[i].deviceIDs) > len(successGroups[bestIdx].deviceIDs) {
				bestIdx = i
			}
		}
		actionResp.UpdatedState = successGroups[bestIdx].state
		for i, g := range successGroups {
			if g.isCamera && i != bestIdx {
				// Never report a camera session's state under a foreign
				// device's id, or a foreign state under this camera's id
				// (see isCamera's doc comment above) - the agent already
				// started the session regardless; it just won't show as
				// "updated" in this particular EXECUTE response, and a
				// retry would succeed.
				continue
			}
			actionResp.UpdatedDevices = append(actionResp.UpdatedDevices, g.deviceIDs...)
		}
	}

	return actionResp, nil
}

var _ action.Provider = (*RemoteProvider)(nil)
