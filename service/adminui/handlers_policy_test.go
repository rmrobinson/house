package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/policy"
)

// noopHomeAPI is a policy.HomeAPI that no test.trigger condition here ever
// calls - it exists only to satisfy policy.NewEngine's constructor.
type noopHomeAPI struct{}

func (noopHomeAPI) GetLight(id string) (bool, error)                  { return false, nil }
func (noopHomeAPI) SetLight(id string, on bool) error                 { return nil }
func (noopHomeAPI) GetSensor(id string) (float64, error)              { return 0, nil }
func (noopHomeAPI) GetState(id, key string) (any, error)              { return nil, nil }
func (noopHomeAPI) SetState(id, key string, value any) error          { return nil }
func (noopHomeAPI) GetHouseState(key string) (any, error)             { return nil, nil }
func (noopHomeAPI) SetHouseState(key string, value any) error         { return nil }
func (noopHomeAPI) GetLastKnown(id string) (any, error)               { return nil, nil }
func (noopHomeAPI) Notify(event string, payload map[string]any) error { return nil }

// testTrigger is a manually-flippable condition, registered under
// "test.trigger" - the adminui-side equivalent of service/policy's own
// (unexported) manualCondition test helper, built here from the exported
// NewPollingCondition since manualCondition isn't visible outside package
// policy.
type testTrigger struct {
	value atomic.Bool
}

func (t *testTrigger) condition() policy.Condition {
	return policy.NewPollingCondition(t.value.Load, time.Millisecond)
}

// startTestPolicyServer wires a real policy.Engine + policy.Service behind
// a real in-process gRPC server (same pattern as service/bridge/facade's
// upstream_test.go), and returns a Server whose handlers this file exercises
// directly - end-to-end from handlers_policy.go's HTTP handler through the
// actual PolicyServiceClient, gRPC, and policy.Service/Engine.
func startTestPolicyServer(t *testing.T) (*Server, *testTrigger) {
	t.Helper()

	registry := policy.NewConditionRegistry()
	trigger := &testTrigger{}
	policy.RegisterConditionType(registry, "test.trigger", func(_ struct{}) policy.Condition { return trigger.condition() })

	engine := policy.NewEngine(noopHomeAPI{}, registry, zaptest.NewLogger(t))
	t.Cleanup(engine.Close)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	grpcServer := grpc.NewServer()
	api2.RegisterPolicyServiceServer(grpcServer, policy.NewService(zaptest.NewLogger(t), engine, registry))
	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.Stop)

	conn, err := grpcutil.DialInsecure(lis.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return &Server{logger: zaptest.NewLogger(t), policy: api2.NewPolicyServiceClient(conn)}, trigger
}

func newTestRequest(method, path string, body string, pathValues map[string]string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	for k, v := range pathValues {
		r.SetPathValue(k, v)
	}
	return r
}

func TestHandlePoliciesListEmpty(t *testing.T) {
	s, _ := startTestPolicyServer(t)

	rec := httptest.NewRecorder()
	s.handlePoliciesList(rec, newTestRequest("GET", "/policies", "", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "No policies registered yet.")
}

func TestHandlePolicyDetailNotFound(t *testing.T) {
	s, _ := startTestPolicyServer(t)

	rec := httptest.NewRecorder()
	s.handlePolicyDetail(rec, newTestRequest("GET", "/policies/does-not-exist", "", map[string]string{"id": "does-not-exist"}))

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func createTestPolicy(t *testing.T, s *Server, id string) {
	t.Helper()
	form := url.Values{
		"id":                 {id},
		"condition_expr":     {`{"type":"use","name":"test.trigger","params":{}}`},
		"script":             {"x=1"},
		"on_condition_false": {"complete"},
		"is_new":             {"true"},
	}
	rec := httptest.NewRecorder()
	s.handlePolicySubmit(rec, newTestRequest("POST", "/policies", form.Encode(), nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "/policies/"+id, rec.Header().Get("HX-Redirect"))
}

func TestHandlePolicySubmitCreatesPolicy(t *testing.T) {
	s, _ := startTestPolicyServer(t)
	createTestPolicy(t, s, "test.created")

	rec := httptest.NewRecorder()
	s.handlePolicyDetail(rec, newTestRequest("GET", "/policies/test.created", "", map[string]string{"id": "test.created"}))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "test.created")
}

// TestHandlePolicyEditorEditPrefillsForm guards against handlePolicyEditorEdit
// regressing to a blank form: it should populate the editor from the
// existing policy's ID/script/condition JSON/on-condition-false, and from
// the concurrently-fetched condition type list.
func TestHandlePolicyEditorEditPrefillsForm(t *testing.T) {
	s, _ := startTestPolicyServer(t)
	createTestPolicy(t, s, "test.editme")

	rec := httptest.NewRecorder()
	s.handlePolicyEditorEdit(rec, newTestRequest("GET", "/policies/test.editme/edit", "", map[string]string{"id": "test.editme"}))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `value="test.editme"`)
	assert.Contains(t, body, "x=1")
	assert.Contains(t, body, "test.trigger")
}

// TestHandlePolicySubmitMalformedRequestShowsFlash guards the ParseForm
// error branch: a request body ParseForm can't decode should still render
// the editor (as a new/blank form) with a flash, not fail some other way.
func TestHandlePolicySubmitMalformedRequestShowsFlash(t *testing.T) {
	s, _ := startTestPolicyServer(t)

	req := httptest.NewRequest("POST", "/policies", strings.NewReader("id=%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	s.handlePolicySubmit(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("HX-Redirect"))
	assert.Contains(t, rec.Body.String(), "invalid form")
	assert.Contains(t, rec.Body.String(), `class="flash flash-error"`)
}

func TestHandlePolicySubmitRejectsBadConditionJSON(t *testing.T) {
	s, _ := startTestPolicyServer(t)

	form := url.Values{
		"id":             {"test.bad"},
		"condition_expr": {`not json`},
		"script":         {"x=1"},
		"is_new":         {"true"},
	}
	rec := httptest.NewRecorder()
	s.handlePolicySubmit(rec, newTestRequest("POST", "/policies", form.Encode(), nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid condition expression")
	assert.Empty(t, rec.Header().Get("HX-Redirect"))
}

func TestHandlePolicyDeleteUnregisters(t *testing.T) {
	s, _ := startTestPolicyServer(t)
	createTestPolicy(t, s, "test.deleteme")

	rec := httptest.NewRecorder()
	s.handlePolicyDelete(rec, newTestRequest("POST", "/policies/test.deleteme/delete", "", map[string]string{"id": "test.deleteme"}))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "/policies", rec.Header().Get("HX-Redirect"))

	rec = httptest.NewRecorder()
	s.handlePolicyDetail(rec, newTestRequest("GET", "/policies/test.deleteme", "", map[string]string{"id": "test.deleteme"}))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandlePolicyDeleteNotRegisteredShowsFlash(t *testing.T) {
	s, _ := startTestPolicyServer(t)

	rec := httptest.NewRecorder()
	s.handlePolicyDelete(rec, newTestRequest("POST", "/policies/does-not-exist/delete", "", map[string]string{"id": "does-not-exist"}))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("HX-Redirect"))
	body := rec.Body.String()
	assert.Contains(t, body, "does-not-exist")
	assert.Contains(t, body, `class="flash flash-error"`)
}

func TestHandlePolicySimulate(t *testing.T) {
	s, trigger := startTestPolicyServer(t)
	createTestPolicy(t, s, "test.sim")

	rec := httptest.NewRecorder()
	s.handlePolicySimulate(rec, newTestRequest("GET", "/policies/test.sim/simulate", "", map[string]string{"id": "test.sim"}))
	assert.Contains(t, rec.Body.String(), "false")

	trigger.value.Store(true)
	require.Eventually(t, func() bool {
		rec := httptest.NewRecorder()
		s.handlePolicySimulate(rec, newTestRequest("GET", "/policies/test.sim/simulate", "", map[string]string{"id": "test.sim"}))
		return strings.Contains(rec.Body.String(), "TRUE")
	}, time.Second, 10*time.Millisecond)
}

func TestHandleLogsPage(t *testing.T) {
	s, trigger := startTestPolicyServer(t)
	createTestPolicy(t, s, "test.logged")

	trigger.value.Store(true)
	require.Eventually(t, func() bool {
		logs, err := s.listLogs(context.Background(), "test.logged")
		require.NoError(t, err)
		return len(logs) == 1
	}, time.Second, 10*time.Millisecond)

	rec := httptest.NewRecorder()
	s.handleLogs(rec, newTestRequest("GET", "/logs", "", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "test.logged")

	rec = httptest.NewRecorder()
	s.handleLogs(rec, newTestRequest("GET", "/logs?policy=nonexistent", "", nil))
	assert.Contains(t, rec.Body.String(), "No executions yet.")
}
