package main

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	api2 "github.com/rmrobinson/house/api"
)

// policyView/executionLogView mirror the PolicyService proto messages,
// reshaped for templates - same convention as buildingView/deviceView in
// view.go, but for policyd's API instead of housed's.
type policyView struct {
	ID                string
	ConditionExprJSON string
	Script            string
	OnConditionFalse  api2.OnConditionFalse
	Active            bool
	LastLog           *executionLogView
}

type executionLogView struct {
	PolicyID  string
	StartedAt time.Time
	EndedAt   time.Time
	Status    string
	Error     string
}

func executionLogToView(l *api2.ExecutionLog) executionLogView {
	v := executionLogView{
		PolicyID: l.GetPolicyId(),
		Status:   l.GetStatus(),
		Error:    l.GetError(),
	}
	if ts := l.GetStartedAt(); ts != nil {
		v.StartedAt = ts.AsTime()
	}
	if ts := l.GetEndedAt(); ts != nil {
		v.EndedAt = ts.AsTime()
	}
	return v
}

func policyToView(p *api2.Policy) policyView {
	v := policyView{
		ID:                p.GetId(),
		ConditionExprJSON: p.GetConditionExprJson(),
		Script:            p.GetScript(),
		OnConditionFalse:  p.GetOnConditionFalse(),
		Active:            p.GetActive(),
	}
	if l := p.GetLastLog(); l != nil {
		lv := executionLogToView(l)
		v.LastLog = &lv
	}
	return v
}

// formatTimeFunc/durationFunc/conditionPrettyFunc/onConditionFalseStrFunc
// back the policy pages' template.FuncMap (see templates.go) - ported from
// service/policy/http.go's identical helpers, retargeted at this package's
// view types/the API's own OnConditionFalse enum instead of the engine's Go
// types, since adminui only ever sees policies through PolicyServiceClient.
func formatTimeFunc(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func durationFunc(l executionLogView) string {
	if l.EndedAt.IsZero() {
		return "running"
	}
	return l.EndedAt.Sub(l.StartedAt).Round(time.Millisecond).String()
}

// conditionPrettyFunc pretty-prints exprJSON - already the exact compact
// JSON policyd's condition-expression wire format produces (see
// api/policy.proto's Policy.condition_expr_json), so this only needs to
// re-indent it, not build it from a Go value the way http.go's version did.
func conditionPrettyFunc(exprJSON string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(exprJSON), "", "  "); err != nil {
		return exprJSON
	}
	return buf.String()
}

func onConditionFalseStrFunc(v api2.OnConditionFalse) string {
	if v == api2.OnConditionFalse_COMPLETE {
		return "complete"
	}
	return "interrupt"
}

var policyTemplateFuncs = map[string]any{
	"formatTime":          formatTimeFunc,
	"duration":            durationFunc,
	"conditionPretty":     conditionPrettyFunc,
	"onConditionFalseStr": onConditionFalseStrFunc,
}

// listPolicies wraps PolicyService.ListPolicies into a plain slice of
// views, sorted by ID server-side already (see service/policy.Engine.
// Policies) - admin-tool traffic, not large enough to justify streaming
// incrementally (matches listBuildings/listFloors in view.go).
func (s *Server) listPolicies(ctx context.Context) ([]policyView, error) {
	items, err := streamAll(func(cb func(*api2.Policy) error) error {
		stream, err := s.policy.ListPolicies(ctx, &api2.ListPoliciesRequest{})
		if err != nil {
			return err
		}
		return recvAll(stream, cb)
	})
	if err != nil {
		return nil, err
	}
	out := make([]policyView, 0, len(items))
	for _, p := range items {
		out = append(out, policyToView(p))
	}
	return out, nil
}

// maxLogRows caps how many execution log rows a single page renders, newest
// first - matches service/policy/http.go's own cap, ported here since
// PolicyService.ListLogs returns oldest-first same as Engine.Logs() did.
const maxLogRows = 200

// listLogs returns up to maxLogRows of policyID's logs (every policy's, if
// policyID is empty), newest first.
func (s *Server) listLogs(ctx context.Context, policyID string) ([]executionLogView, error) {
	items, err := streamAll(func(cb func(*api2.ExecutionLog) error) error {
		stream, err := s.policy.ListLogs(ctx, &api2.ListLogsRequest{PolicyId: policyID})
		if err != nil {
			return err
		}
		return recvAll(stream, cb)
	})
	if err != nil {
		return nil, err
	}

	out := make([]executionLogView, 0, min(len(items), maxLogRows))
	for i := len(items) - 1; i >= 0 && len(out) < maxLogRows; i-- {
		out = append(out, executionLogToView(items[i]))
	}
	return out, nil
}

func (s *Server) listConditionTypes(ctx context.Context) ([]string, error) {
	resp, err := s.policy.ListConditionTypes(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, err
	}
	return resp.GetTypeNames(), nil
}
