package policy

import (
	"context"
	"errors"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
)

// Service implements api2.PolicyServiceServer over an Engine - the gRPC
// analogue of service/house.Service, exposing exactly the introspection/
// mutation surface an HTTP UI needs (see service/adminui's policy pages)
// without that UI living in-process with the engine itself. registry is
// used to build/validate a SavePolicy request's condition_expr_json and to
// answer ListConditionTypes.
type Service struct {
	logger   *zap.Logger
	engine   *Engine
	registry *ConditionRegistry
}

// NewService creates a Service serving engine's policies, backed by
// registry for condition-expression (de)serialization.
func NewService(logger *zap.Logger, engine *Engine, registry *ConditionRegistry) *Service {
	return &Service{logger: logger, engine: engine, registry: registry}
}

// executionLogToAPI converts l to its wire representation. EndedAt is left
// unset (nil) while l is still running, matching ExecutionLog's own
// zero-value convention.
func executionLogToAPI(l ExecutionLog) *api2.ExecutionLog {
	out := &api2.ExecutionLog{
		PolicyId:  l.PolicyID,
		StartedAt: timestamppb.New(l.StartedAt),
		Status:    string(l.Status),
		Error:     l.Error,
	}
	if !l.EndedAt.IsZero() {
		out.EndedAt = timestamppb.New(l.EndedAt)
	}
	return out
}

func onConditionFalseToAPI(v OnConditionFalse) api2.OnConditionFalse {
	if v == Complete {
		return api2.OnConditionFalse_COMPLETE
	}
	return api2.OnConditionFalse_INTERRUPT
}

func onConditionFalseFromAPI(v api2.OnConditionFalse) OnConditionFalse {
	if v == api2.OnConditionFalse_COMPLETE {
		return Complete
	}
	return Interrupt
}

// policyInfoToAPI converts info to its wire representation. lastLog, if
// non-nil, populates Policy.last_log (ListPolicies only - see its doc
// comment on api/policy.proto's Policy message).
func (s *Service) policyInfoToAPI(info PolicyInfo, lastLog *ExecutionLog) (*api2.Policy, error) {
	exprJSON, err := MarshalConditionExpr(info.ConditionExpr)
	if err != nil {
		s.logger.Error("unable to marshal condition expression", zap.String("policy_id", info.ID), zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to marshal condition expression")
	}

	out := &api2.Policy{
		Id:                info.ID,
		ConditionExprJson: string(exprJSON),
		Script:            info.Script,
		OnConditionFalse:  onConditionFalseToAPI(info.OnConditionFalse),
		Active:            info.Active,
	}
	if lastLog != nil {
		out.LastLog = executionLogToAPI(*lastLog)
	}
	return out, nil
}

// ListPolicies streams every registered policy, each with LastLog already
// populated from a single LastLogs() pass - mirroring the join
// service/adminui's policies list page needs, without a second RPC per row.
func (s *Service) ListPolicies(req *api2.ListPoliciesRequest, stream api2.PolicyService_ListPoliciesServer) error {
	infos := s.engine.Policies()
	lastLogs := s.engine.LastLogs()

	for _, info := range infos {
		var last *ExecutionLog
		if l, ok := lastLogs[info.ID]; ok {
			last = &l
		}
		p, err := s.policyInfoToAPI(info, last)
		if err != nil {
			return err
		}
		if err := stream.Send(p); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) GetPolicy(ctx context.Context, req *api2.GetPolicyRequest) (*api2.Policy, error) {
	info, ok := s.engine.Policy(req.GetId())
	if !ok {
		return nil, status.Error(codes.NotFound, "policy doesn't exist")
	}
	var last *ExecutionLog
	if l, ok := s.engine.LastLog(req.GetId()); ok {
		last = &l
	}
	return s.policyInfoToAPI(info, last)
}

// SavePolicy upserts a policy, matching Engine.Register's own
// create-or-replace semantics.
func (s *Service) SavePolicy(ctx context.Context, req *api2.SavePolicyRequest) (*api2.Policy, error) {
	expr, err := UnmarshalConditionExpr([]byte(req.GetConditionExprJson()), s.registry)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid condition expression: %v", err)
	}

	p := &Policy{
		ID:               req.GetId(),
		ConditionExpr:    expr,
		Script:           req.GetScript(),
		OnConditionFalse: onConditionFalseFromAPI(req.GetOnConditionFalse()),
	}
	if err := s.engine.Register(p); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	info, ok := s.engine.Policy(req.GetId())
	if !ok {
		// Register just succeeded above; only a concurrent Unregister of
		// this same id, racing right after, gets here. Reporting it as
		// Internal rather than fabricating a response from a zero-value
		// info: the save itself worked, but what the engine now holds for
		// id is "nothing", not "what was just saved".
		return nil, status.Errorf(codes.Internal, "policy %q: saved but no longer registered (removed concurrently)", req.GetId())
	}
	return s.policyInfoToAPI(info, nil)
}

func (s *Service) DeletePolicy(ctx context.Context, req *api2.DeletePolicyRequest) (*emptypb.Empty, error) {
	if err := s.engine.Unregister(req.GetId()); err != nil {
		if errors.Is(err, ErrPolicyNotRegistered) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		s.logger.Error("unable to unregister policy", zap.String("policy_id", req.GetId()), zap.Error(err))
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &emptypb.Empty{}, nil
}

func (s *Service) SimulatePolicy(ctx context.Context, req *api2.SimulatePolicyRequest) (*api2.SimulatePolicyResponse, error) {
	result, registered := s.engine.Simulate(req.GetId())
	return &api2.SimulatePolicyResponse{Result: result, Registered: registered}, nil
}

// ListLogs streams up to req.Limit ExecutionLogs matching req, newest first
// (see Engine.RecentLogs) - bounded server-side so a caller wanting only the
// most recent rows (e.g. a UI) doesn't pay for streaming and buffering the
// entire matching history just to truncate it client-side.
func (s *Service) ListLogs(req *api2.ListLogsRequest, stream api2.PolicyService_ListLogsServer) error {
	logs, err := s.engine.RecentLogs(req.GetPolicyId(), int(req.GetLimit()))
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	for _, l := range logs {
		if err := stream.Send(executionLogToAPI(l)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ListConditionTypes(ctx context.Context, req *emptypb.Empty) (*api2.ListConditionTypesResponse, error) {
	return &api2.ListConditionTypesResponse{TypeNames: s.registry.TypeNames()}, nil
}

// StreamEvents relays the engine's UI-facing Bus topics as PolicyEvents for
// the lifetime of the call - the gRPC analogue of BridgeService.
// StreamUpdates, meant to be fanned out by a single upstream subscription
// (see service/adminui/hub.go's deviceHub) rather than one per browser tab.
//
// A uiLogAppendedTopic event emits two PolicyEvents, mirroring the two
// visual effects service/policy/http.go's pushLogAppended used to combine
// into one HTML fragment: the appended log itself, and (if the policy is
// still registered) its refreshed Policy snapshot, since a run's outcome
// also changes what a status cell shows.
func (s *Service) StreamEvents(req *api2.StreamEventsRequest, stream api2.PolicyService_StreamEventsServer) error {
	policyCh := s.engine.Bus().Subscribe(uiPolicyChangedTopic)
	defer s.engine.Bus().Unsubscribe(uiPolicyChangedTopic, policyCh)

	logCh := s.engine.Bus().Subscribe(uiLogAppendedTopic)
	defer s.engine.Bus().Unsubscribe(uiLogAppendedTopic, logCh)

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-policyCh:
			if !ok {
				return nil
			}
			id, castOk := ev.Payload.(string)
			if !castOk {
				continue
			}
			if err := s.sendPolicyChanged(stream, id); err != nil {
				return err
			}

		case ev, ok := <-logCh:
			if !ok {
				return nil
			}
			l, castOk := ev.Payload.(ExecutionLog)
			if !castOk {
				continue
			}
			if err := stream.Send(&api2.PolicyEvent{Event: &api2.PolicyEvent_LogAppended{LogAppended: executionLogToAPI(l)}}); err != nil {
				return err
			}
			if err := s.sendPolicyChanged(stream, l.PolicyID); err != nil {
				return err
			}
		}
	}
}

// sendPolicyChanged sends id's current Policy snapshot as a policy_changed
// event, or a policy_removed event if id is no longer registered.
func (s *Service) sendPolicyChanged(stream api2.PolicyService_StreamEventsServer, id string) error {
	info, ok := s.engine.Policy(id)
	if !ok {
		return stream.Send(&api2.PolicyEvent{Event: &api2.PolicyEvent_PolicyRemoved{PolicyRemoved: id}})
	}
	p, err := s.policyInfoToAPI(info, nil)
	if err != nil {
		return err
	}
	return stream.Send(&api2.PolicyEvent{Event: &api2.PolicyEvent_PolicyChanged{PolicyChanged: p}})
}
