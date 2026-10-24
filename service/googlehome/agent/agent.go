package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	action "github.com/rmrobinson/google-smart-home-action-go"
	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/googlehome/relay"
	"github.com/rmrobinson/house/service/lib/bridgeconn"
	"github.com/rmrobinson/house/service/lib/grpcutil"
)

// executeCommandTimeout bounds each ExecuteCommand call this Agent issues
// against BridgeService - see service/policy/bridgehome's commandTimeout
// doc comment for the same reasoning (must clear the slowest bridge's own
// confirmation wait).
const executeCommandTimeout = 15 * time.Second

// commandOrigin is stamped on every command this agent issues, so it's
// distinguishable from UI/policy-driven commands on the BridgeService
// update stream.
const commandOrigin = "googlehome-agentd"

// updateQueueSize bounds the channel handleBridgeUpdate feeds - see
// bridgehome.Adapter's updateQueueSize doc comment for why this needs to be
// a queue rather than applying updates inline from the Recv loop.
const updateQueueSize = 256

// Agent translates between one BridgeService connection (housed or
// bridgefacaded) and one RelayService stream to googlehomed: BridgeService
// Update notifications become RequestSync/ReportState calls up to the
// relay, and SYNC/QUERY/EXECUTE requests pushed down from the relay become
// BridgeService calls.
type Agent struct {
	logger      *zap.Logger
	agentID     string
	bridgeConn  *bridgeconn.Conn
	relayClient relay.RelayServiceClient

	updates chan *api2.Update

	mu                 sync.Mutex
	unsupportedDevices map[string]bool
}

// New creates an Agent that will connect to bridgeAddr (BridgeService) and
// use relayClient to reach googlehomed once Run is called, registering as
// agentID.
func New(logger *zap.Logger, bridgeAddr string, bridgeTLS *grpcutil.ClientTLSConfig, relayClient relay.RelayServiceClient, agentID string) *Agent {
	return &Agent{
		logger:             logger,
		agentID:            agentID,
		bridgeConn:         bridgeconn.New(logger, bridgeAddr, bridgeTLS),
		relayClient:        relayClient,
		updates:            make(chan *api2.Update, updateQueueSize),
		unsupportedDevices: map[string]bool{},
	}
}

// Run opens the relay stream, registers as a.agentID, starts the
// (reconnecting) BridgeService connection, and translates between the two
// until ctx is cancelled or the relay stream ends.
func (a *Agent) Run(ctx context.Context) error {
	stream, err := a.relayClient.StateSync(ctx)
	if err != nil {
		return fmt.Errorf("agent: unable to open relay stream: %w", err)
	}

	if err := stream.Send(&relay.ClientRequest{
		RequestId: uuid.New().String(),
		Field:     &relay.ClientRequest_RegisterAgent{RegisterAgent: &relay.RegisterAgentRequest{AgentId: a.agentID}},
	}); err != nil {
		return fmt.Errorf("agent: unable to register with relay: %w", err)
	}

	go a.bridgeConn.Run(ctx, a.handleBridgeUpdate, nil)
	go a.processUpdates(ctx, stream)

	return a.processRelayRequests(ctx, stream)
}

// handleBridgeUpdate is bridgeconn.Conn.Run's onUpdate callback - called
// synchronously from its own Recv loop, so it must return immediately. A
// full queue drops the update rather than blocking: a dropped CHANGED just
// delays that device's state reaching HomeGraph until its next change (or
// the next QUERY), and a dropped ADDED/REMOVED only delays a RequestSync
// that a later one will trigger anyway.
func (a *Agent) handleBridgeUpdate(u *api2.Update) {
	select {
	case a.updates <- u:
	default:
		a.logger.Warn("update queue full, dropping bridge update")
	}
}

// processUpdates applies every queued BridgeService Update on its own
// goroutine, so a burst (e.g. a facade's INITIAL replay) never blocks
// bridgeConn's Recv loop.
func (a *Agent) processUpdates(ctx context.Context, stream relay.RelayService_StateSyncClient) {
	for {
		select {
		case <-ctx.Done():
			return
		case u := <-a.updates:
			a.applyBridgeUpdate(stream, u)
		}
	}
}

// applyBridgeUpdate sends a RequestSync up to the relay when a device is
// added or removed (Google needs to re-poll SYNC to learn about it), or a
// ReportState when a supported device's state changes. Updates for a
// device kind translate.go doesn't support are silently skipped - most
// devices in a house aren't lights, and that's expected, not an error.
func (a *Agent) applyBridgeUpdate(stream relay.RelayService_StateSyncClient, u *api2.Update) {
	du := u.GetDeviceUpdate()
	if du == nil {
		return
	}

	switch u.Action {
	case api2.Update_ADDED, api2.Update_REMOVED:
		if err := stream.Send(&relay.ClientRequest{
			RequestId: uuid.New().String(),
			Field:     &relay.ClientRequest_RequestSync{RequestSync: &relay.RequestSyncRequest{}},
		}); err != nil {
			a.logger.Error("unable to request sync", zap.Error(err))
		}
	case api2.Update_CHANGED:
		d := du.GetDevice()
		if d == nil {
			return
		}

		a.mu.Lock()
		unsupported := a.unsupportedDevices[d.GetId()]
		a.mu.Unlock()
		if unsupported {
			return
		}

		state, err := stateToAction(d)
		if err != nil {
			return
		}

		payload, err := json.Marshal(map[string]action.DeviceState{d.GetId(): state})
		if err != nil {
			a.logger.Error("unable to serialize device state", zap.String("device_id", d.GetId()), zap.Error(err))
			return
		}
		if err := stream.Send(&relay.ClientRequest{
			RequestId: uuid.New().String(),
			Field:     &relay.ClientRequest_ReportState{ReportState: &relay.ReportStateRequest{Payload: payload}},
		}); err != nil {
			a.logger.Error("unable to report state", zap.String("device_id", d.GetId()), zap.Error(err))
		}
	}
}

// processRelayRequests handles every SYNC/QUERY/EXECUTE/disconnect request
// googlehomed sends down, replying on the same stream, until it ends.
func (a *Agent) processRelayRequests(ctx context.Context, stream relay.RelayService_StateSyncClient) error {
	// Google re-syncs on request; ask once up front too, so this agent
	// restarting picks up any device changes it missed while it was down.
	if err := stream.Send(&relay.ClientRequest{
		RequestId: uuid.New().String(),
		Field:     &relay.ClientRequest_RequestSync{RequestSync: &relay.RequestSyncRequest{}},
	}); err != nil {
		a.logger.Error("unable to request initial sync", zap.Error(err))
	}

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		var sendErr error
		switch {
		case req.GetDisconnectRequest() != nil:
			return nil
		case req.GetSyncRequest() != nil:
			resp := a.processSyncRequest(ctx)
			sendErr = stream.Send(&relay.ClientRequest{RequestId: req.RequestId, Field: &relay.ClientRequest_SyncResponse{SyncResponse: resp}})
		case req.GetQueryRequest() != nil:
			resp := a.processQueryRequest(ctx, req.GetQueryRequest())
			sendErr = stream.Send(&relay.ClientRequest{RequestId: req.RequestId, Field: &relay.ClientRequest_QueryResponse{QueryResponse: resp}})
		case req.GetExecuteRequest() != nil:
			resp := a.processExecuteRequest(ctx, req.GetExecuteRequest())
			sendErr = stream.Send(&relay.ClientRequest{RequestId: req.RequestId, Field: &relay.ClientRequest_ExecuteResponse{ExecuteResponse: resp}})
		default:
			a.logger.Info("received unhandled relay request type")
			continue
		}
		if sendErr != nil {
			a.logger.Error("unable to send relay response", zap.String("request_id", req.RequestId), zap.Error(sendErr))
		}
	}
}

func (a *Agent) processSyncRequest(ctx context.Context) *relay.SyncResponse {
	client := a.bridgeConn.Client()
	if client == nil {
		return &relay.SyncResponse{ErrorDetails: "bridge connection not ready"}
	}

	listResp, err := client.ListDevices(ctx, &api2.ListDevicesRequest{})
	if err != nil {
		return &relay.SyncResponse{ErrorDetails: err.Error()}
	}

	var devices []*action.Device
	a.mu.Lock()
	for _, d := range listResp.GetDevices() {
		ad, ok := deviceToAction(d)
		if !ok {
			a.unsupportedDevices[d.GetId()] = true
			continue
		}
		delete(a.unsupportedDevices, d.GetId())
		devices = append(devices, ad)
	}
	a.mu.Unlock()

	payload, err := json.Marshal(devices)
	if err != nil {
		return &relay.SyncResponse{ErrorDetails: err.Error()}
	}
	return &relay.SyncResponse{Payload: payload}
}

func (a *Agent) processQueryRequest(ctx context.Context, req *relay.QueryRequest) *relay.QueryResponse {
	client := a.bridgeConn.Client()
	if client == nil {
		return &relay.QueryResponse{ErrorDetails: "bridge connection not ready"}
	}

	states := map[string]action.DeviceState{}
	for _, id := range req.GetDeviceIds() {
		d, err := client.GetDevice(ctx, &api2.GetDeviceRequest{Id: id})
		if err != nil {
			states[id] = action.NewDeviceState(false)
			continue
		}
		state, err := stateToAction(d)
		if err != nil {
			states[id] = action.NewDeviceState(false)
			continue
		}
		states[id] = state
	}

	payload, err := json.Marshal(states)
	if err != nil {
		return &relay.QueryResponse{ErrorDetails: err.Error()}
	}
	return &relay.QueryResponse{Payload: payload}
}

func (a *Agent) processExecuteRequest(ctx context.Context, req *relay.ExecuteRequest) *relay.ExecuteResponse {
	client := a.bridgeConn.Client()
	if client == nil {
		return &relay.ExecuteResponse{ErrorDetails: "bridge connection not ready"}
	}

	resp := &relay.ExecuteResponse{}
	for _, cmdGroup := range req.GetCommands() {
		var actionCmds []action.Command
		for _, ec := range cmdGroup.GetExecutionContext() {
			var cmd action.Command
			if err := json.Unmarshal(ec.GetPayload(), &cmd); err != nil {
				a.logger.Error("unable to deserialize command", zap.Error(err))
				continue
			}
			actionCmds = append(actionCmds, cmd)
		}

		for _, deviceID := range cmdGroup.GetDeviceIds() {
			a.executeOne(ctx, client, deviceID, actionCmds, resp)
		}
	}
	return resp
}

// executeOne runs every command in cmds, in order, against deviceID and
// appends exactly one CommandResult to resp - SUCCESS (with the device's
// resulting state) if every command applied, OFFLINE if the device
// couldn't be found, or ERROR (with a Google-recognized errorCode) if a
// command isn't supported or the bridge rejected it.
func (a *Agent) executeOne(ctx context.Context, client api2.BridgeServiceClient, deviceID string, cmds []action.Command, resp *relay.ExecuteResponse) {
	cmdCtx, cancel := context.WithTimeout(ctx, executeCommandTimeout)
	defer cancel()

	d, err := client.GetDevice(cmdCtx, &api2.GetDeviceRequest{Id: deviceID})
	if err != nil {
		appendResult(resp, relay.ExecuteResponse_OFFLINE, deviceID, "")
		return
	}

	current := d
	for _, cmd := range cmds {
		cc, ok := buildCommand(current, deviceID, cmd)
		if !ok {
			appendResult(resp, relay.ExecuteResponse_ERROR, deviceID, "notSupported")
			return
		}
		cc.Origin = commandOrigin

		updated, err := client.ExecuteCommand(cmdCtx, cc)
		if err != nil {
			a.logger.Info("execute command failed", zap.String("device_id", deviceID), zap.Error(err))
			appendResult(resp, relay.ExecuteResponse_ERROR, deviceID, "commandInsertFailed")
			return
		}
		current = updated
	}

	state, err := stateToAction(current)
	if err != nil {
		appendResult(resp, relay.ExecuteResponse_ERROR, deviceID, "commandInsertFailed")
		return
	}
	payload, err := json.Marshal(state)
	if err != nil {
		appendResult(resp, relay.ExecuteResponse_ERROR, deviceID, "commandInsertFailed")
		return
	}
	resp.Results = append(resp.Results, &relay.ExecuteResponse_CommandResult{
		DeviceIds: []string{deviceID},
		Status:    relay.ExecuteResponse_SUCCESS,
		States:    payload,
	})
}

func appendResult(resp *relay.ExecuteResponse, status relay.ExecuteResponse_CommandStatus, deviceID, errCode string) {
	resp.Results = append(resp.Results, &relay.ExecuteResponse_CommandResult{
		DeviceIds: []string{deviceID},
		Status:    status,
		ErrorCode: errCode,
	})
}
