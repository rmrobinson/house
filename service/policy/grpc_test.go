package policy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
)

func newTestService(t *testing.T) (*Service, *Engine, *ConditionRegistry) {
	t.Helper()
	e, r := newTestEngine(t, newFakeHomeAPI())
	return NewService(zaptest.NewLogger(t), e, r), e, r
}

// fakeStream is a minimal grpc.ServerStream stand-in for exercising a
// server-streaming RPC without a real network connection - same pattern as
// service/house/service_test.go's fakeListRoomsServer, generic over the sent
// message type.
type fakeStream[T any] struct {
	grpc.ServerStream
	ctx context.Context

	mu   sync.Mutex
	sent []T
}

func (s *fakeStream[T]) Context() context.Context { return s.ctx }
func (s *fakeStream[T]) Send(m T) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}

// Sent returns a snapshot of every message sent so far, safe to call
// concurrently with the RPC's own goroutine still calling Send.
func (s *fakeStream[T]) Sent() []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]T, len(s.sent))
	copy(out, s.sent)
	return out
}

func newFakeStream[T any](ctx context.Context) *fakeStream[T] {
	if ctx == nil {
		ctx = context.Background()
	}
	return &fakeStream[T]{ctx: ctx}
}

// waitForSubscriber blocks until bus has at least one subscriber on topic -
// StreamEvents runs in its own goroutine (see the StreamEvents tests below),
// and Bus.Publish doesn't buffer for a subscriber that hasn't called
// Subscribe yet, so a mutation made before that happens would otherwise be
// silently missed.
func waitForSubscriber(t *testing.T, bus *Bus, topic string) {
	t.Helper()
	require.Eventually(t, func() bool {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		return len(bus.subs[topic]) > 0
	}, time.Second, time.Millisecond)
}

func TestListPoliciesEmpty(t *testing.T) {
	s, _, _ := newTestService(t)

	stream := newFakeStream[*api2.Policy](nil)
	require.NoError(t, s.ListPolicies(&api2.ListPoliciesRequest{}, stream))
	assert.Empty(t, stream.Sent())
}

func TestGetPolicyNotFound(t *testing.T) {
	s, _, _ := newTestService(t)

	_, err := s.GetPolicy(context.Background(), &api2.GetPolicyRequest{Id: "does-not-exist"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestGetPolicyFound(t *testing.T) {
	s, e, r := newTestService(t)
	registerManualTrigger(t, r, "trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.detail",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        `home.setLight("x", true)`,
	}))

	p, err := s.GetPolicy(context.Background(), &api2.GetPolicyRequest{Id: "test.detail"})
	require.NoError(t, err)
	assert.Equal(t, "test.detail", p.GetId())
	assert.Equal(t, `home.setLight("x", true)`, p.GetScript())
	assert.False(t, p.GetActive())
	assert.Contains(t, p.GetConditionExprJson(), `"trigger"`)
}

// TestGetPolicyIncludesLastLog guards against a bug where GetPolicy always
// passed a nil lastLog to policyInfoToAPI, so the policy detail page always
// showed "never run" regardless of actual execution history - only
// ListPolicies populated Policy.last_log.
func TestGetPolicyIncludesLastLog(t *testing.T) {
	s, e, r := newTestService(t)
	trigger := registerManualTrigger(t, r, "trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.lastlog",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	trigger.set(true)
	require.Eventually(t, func() bool {
		return len(e.LogsForPolicy("test.lastlog")) == 1
	}, time.Second, 10*time.Millisecond)

	p, err := s.GetPolicy(context.Background(), &api2.GetPolicyRequest{Id: "test.lastlog"})
	require.NoError(t, err)
	require.NotNil(t, p.GetLastLog(), "GetPolicy should populate LastLog once the policy has run, same as ListPolicies")
	assert.Equal(t, "test.lastlog", p.GetLastLog().GetPolicyId())
}

func TestSavePolicyCreatesPolicy(t *testing.T) {
	s, e, r := newTestService(t)
	registerManualTrigger(t, r, "trigger")

	p, err := s.SavePolicy(context.Background(), &api2.SavePolicyRequest{
		Id:                "test.created",
		ConditionExprJson: `{"type":"use","name":"trigger","params":{}}`,
		Script:            "x=1",
		OnConditionFalse:  api2.OnConditionFalse_COMPLETE,
	})
	require.NoError(t, err)
	assert.Equal(t, "test.created", p.GetId())

	info, ok := e.Policy("test.created")
	require.True(t, ok)
	assert.Equal(t, "x=1", info.Script)
	assert.Equal(t, Complete, info.OnConditionFalse)
}

func TestSavePolicyRejectsBadConditionJSON(t *testing.T) {
	s, e, _ := newTestService(t)

	_, err := s.SavePolicy(context.Background(), &api2.SavePolicyRequest{
		Id:                "test.bad",
		ConditionExprJson: `not json`,
		Script:            "x=1",
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	_, ok := e.Policy("test.bad")
	assert.False(t, ok)
}

func TestDeletePolicyUnregisters(t *testing.T) {
	s, e, r := newTestService(t)
	registerManualTrigger(t, r, "trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.deleteme",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	_, err := s.DeletePolicy(context.Background(), &api2.DeletePolicyRequest{Id: "test.deleteme"})
	require.NoError(t, err)

	_, ok := e.Policy("test.deleteme")
	assert.False(t, ok)
}

func TestDeletePolicyNotRegistered(t *testing.T) {
	s, _, _ := newTestService(t)

	_, err := s.DeletePolicy(context.Background(), &api2.DeletePolicyRequest{Id: "does-not-exist"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestSimulatePolicy(t *testing.T) {
	s, e, r := newTestService(t)
	trigger := registerManualTrigger(t, r, "trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.sim",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	resp, err := s.SimulatePolicy(context.Background(), &api2.SimulatePolicyRequest{Id: "test.sim"})
	require.NoError(t, err)
	assert.True(t, resp.GetRegistered())
	assert.False(t, resp.GetResult())

	trigger.set(true)
	require.Eventually(t, func() bool {
		resp, err := s.SimulatePolicy(context.Background(), &api2.SimulatePolicyRequest{Id: "test.sim"})
		require.NoError(t, err)
		return resp.GetResult()
	}, time.Second, 10*time.Millisecond)
}

func TestListLogs(t *testing.T) {
	s, e, r := newTestService(t)
	trigger := registerManualTrigger(t, r, "trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.logged",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))
	trigger.set(true)
	require.Eventually(t, func() bool {
		return len(e.LogsForPolicy("test.logged")) == 1
	}, time.Second, 10*time.Millisecond)

	stream := newFakeStream[*api2.ExecutionLog](nil)
	require.NoError(t, s.ListLogs(&api2.ListLogsRequest{}, stream))
	sent := stream.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, "test.logged", sent[0].GetPolicyId())

	stream = newFakeStream[*api2.ExecutionLog](nil)
	require.NoError(t, s.ListLogs(&api2.ListLogsRequest{PolicyId: "nonexistent"}, stream))
	assert.Empty(t, stream.Sent())
}

// TestListLogsRespectsLimit guards the server-side bound added so a caller
// (e.g. adminui's logs page) doesn't have to stream and buffer every
// matching log just to keep the newest few.
func TestListLogsRespectsLimit(t *testing.T) {
	s, e, r := newTestService(t)

	for i, id := range []string{"test.limited.a", "test.limited.b", "test.limited.c"} {
		trigger := registerManualTrigger(t, r, "trigger."+id)
		require.NoError(t, e.Register(&Policy{
			ID:            id,
			ConditionExpr: Use("trigger."+id, struct{}{}),
			Script:        "x=1",
		}))
		trigger.set(true)
		require.Eventually(t, func() bool {
			return len(e.LogsForPolicy(id)) == 1
		}, time.Second, 10*time.Millisecond, "policy %d", i)
	}

	stream := newFakeStream[*api2.ExecutionLog](nil)
	require.NoError(t, s.ListLogs(&api2.ListLogsRequest{Limit: 2}, stream))
	assert.Len(t, stream.Sent(), 2)
}

func TestListConditionTypes(t *testing.T) {
	s, _, r := newTestService(t)
	registerManualTrigger(t, r, "trigger")

	resp, err := s.ListConditionTypes(context.Background(), nil)
	require.NoError(t, err)
	assert.Contains(t, resp.GetTypeNames(), "trigger")
}

func TestStreamEventsPushesPolicyChangedOnActivation(t *testing.T) {
	s, e, r := newTestService(t)
	trigger := registerManualTrigger(t, r, "trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.sse",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream[*api2.PolicyEvent](ctx)

	done := make(chan error, 1)
	go func() { done <- s.StreamEvents(&api2.StreamEventsRequest{}, stream) }()
	waitForSubscriber(t, e.Bus(), uiPolicyChangedTopic)

	trigger.set(true)

	// Two events follow, in some order: the activation flip's policy_changed,
	// and (once the script finishes) the appended log's log_appended plus its
	// own policy_changed.
	require.Eventually(t, func() bool {
		return len(stream.Sent()) >= 2
	}, time.Second, 10*time.Millisecond)

	var sawActive, sawLog bool
	for _, ev := range stream.Sent() {
		if pc := ev.GetPolicyChanged(); pc != nil && pc.GetActive() {
			sawActive = true
		}
		if ev.GetLogAppended() != nil {
			sawLog = true
		}
	}
	assert.True(t, sawActive, "expected a policy_changed event with active=true")
	assert.True(t, sawLog, "expected a log_appended event")

	cancel()
	require.NoError(t, <-done)
}

func TestStreamEventsPushesPolicyRemovedOnUnregister(t *testing.T) {
	s, e, r := newTestService(t)
	registerManualTrigger(t, r, "trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.gone",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream[*api2.PolicyEvent](ctx)

	done := make(chan error, 1)
	go func() { done <- s.StreamEvents(&api2.StreamEventsRequest{}, stream) }()
	waitForSubscriber(t, e.Bus(), uiPolicyChangedTopic)

	require.NoError(t, e.Unregister("test.gone"))

	require.Eventually(t, func() bool {
		for _, ev := range stream.Sent() {
			if ev.GetPolicyRemoved() == "test.gone" {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond)

	cancel()
	require.NoError(t, <-done)
}
