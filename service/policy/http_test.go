package policy

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func newTestServer(t *testing.T) (*Server, *Engine, *ConditionRegistry) {
	t.Helper()
	e, r := newTestEngine(t, newFakeHomeAPI())
	return NewServer(e, r, zaptest.NewLogger(t)), e, r
}

func TestHandlePoliciesListEmpty(t *testing.T) {
	s, _, _ := newTestServer(t)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/policies", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "No policies registered yet.")
}

func TestHandlePolicyDetailNotFound(t *testing.T) {
	s, _, _ := newTestServer(t)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/policies/does-not-exist", nil))

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandlePolicyDetailFound(t *testing.T) {
	s, e, r := newTestServer(t)
	registerManualTrigger(t, r, "trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.detail",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        `home.setLight("x", true)`,
	}))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/policies/test.detail", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "test.detail")
	assert.Contains(t, body, `home.setLight(&#34;x&#34;, true)`)
	assert.Contains(t, body, "idle") // condition never fired
}

func TestHandlePolicySubmitCreatesPolicy(t *testing.T) {
	s, e, r := newTestServer(t)
	registerManualTrigger(t, r, "trigger")

	form := url.Values{
		"id":                 {"test.created"},
		"condition_expr":     {`{"type":"use","name":"trigger","params":{}}`},
		"script":             {"x=1"},
		"on_condition_false": {"complete"},
		"is_new":             {"true"},
	}
	req := httptest.NewRequest("POST", "/policies", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "/policies/test.created", rec.Header().Get("HX-Redirect"))

	info, ok := e.Policy("test.created")
	require.True(t, ok)
	assert.Equal(t, "x=1", info.Script)
	assert.Equal(t, Complete, info.OnConditionFalse)
}

func TestHandlePolicySubmitRejectsBadConditionJSON(t *testing.T) {
	s, e, _ := newTestServer(t)

	form := url.Values{
		"id":             {"test.bad"},
		"condition_expr": {`not json`},
		"script":         {"x=1"},
		"is_new":         {"true"},
	}
	req := httptest.NewRequest("POST", "/policies", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid condition expression")

	_, ok := e.Policy("test.bad")
	assert.False(t, ok)
}

func TestHandlePolicyEditorEditPrefillsForm(t *testing.T) {
	s, e, r := newTestServer(t)
	registerManualTrigger(t, r, "trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.editme",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=2",
	}))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/policies/test.editme/edit", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "test.editme")
	assert.Contains(t, body, "x=2")
	assert.Contains(t, body, `&#34;trigger&#34;`)
}

func TestHandlePolicyDeleteUnregisters(t *testing.T) {
	s, e, r := newTestServer(t)
	registerManualTrigger(t, r, "trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.deleteme",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/policies/test.deleteme/delete", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "/policies", rec.Header().Get("HX-Redirect"))
	_, ok := e.Policy("test.deleteme")
	assert.False(t, ok)
}

func TestHandlePolicyDeleteNotRegisteredShowsFlashInsteadOfSilentError(t *testing.T) {
	s, _, _ := newTestServer(t)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/policies/does-not-exist/delete", nil))

	// http.Error would return 200 as OK here too but with a bare text body
	// htmx's default responseHandling never swaps into the target — the
	// point of this test is that there IS a visible flash instead.
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("HX-Redirect"))
	body := rec.Body.String()
	assert.Contains(t, body, "does-not-exist")
	assert.Contains(t, body, "not registered")
	assert.Contains(t, body, `class="flash flash-error"`)
}

func TestHandlePolicySubmitMalformedRequestShowsFlash(t *testing.T) {
	s, _, _ := newTestServer(t)

	// An invalid percent-escape in the URL query makes ParseForm fail
	// before it ever gets to look at the POST body.
	req := httptest.NewRequest("POST", "/policies?%zz", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "invalid form")
	assert.Contains(t, body, `class="flash flash-error"`)
}

func TestHandlePolicySimulate(t *testing.T) {
	s, e, r := newTestServer(t)
	trigger := registerManualTrigger(t, r, "trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.sim",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/policies/test.sim/simulate", nil))
	assert.Contains(t, rec.Body.String(), "false")

	trigger.set(true)
	require.Eventually(t, func() bool {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/policies/test.sim/simulate", nil))
		return strings.Contains(rec.Body.String(), "TRUE")
	}, time.Second, 10*time.Millisecond)
}

func TestHandleLogsPage(t *testing.T) {
	s, e, r := newTestServer(t)
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

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/logs", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "test.logged")

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/logs?policy=nonexistent", nil))
	assert.Contains(t, rec.Body.String(), "No executions yet.")
}

// readSSEEvent reads a single SSE event (an "event:" line, a "data:" line,
// terminated by a blank line — see writeSSEEvent) from r and returns its
// data payload.
func readSSEEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()

	var data string
	for {
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		line = strings.TrimRight(line, "\n")
		if line == "" {
			break
		}
		if rest, ok := strings.CutPrefix(line, "data: "); ok {
			data = rest
		}
	}
	return data
}

func TestEventsPushesPolicyStatusOnActivation(t *testing.T) {
	s, e, r := newTestServer(t)
	trigger := registerManualTrigger(t, r, "trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.sse",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	server := httptest.NewServer(s.Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/events")
	require.NoError(t, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)

	trigger.set(true)

	// Two pushes follow, in some order: the activation flip's status patch,
	// and (once the script finishes) the appended log's status-plus-row
	// patch — so check their combined content rather than each one's exact
	// shape.
	first := readSSEEvent(t, reader)
	second := readSSEEvent(t, reader)
	combined := first + second

	assert.Contains(t, combined, `id="policy-status-test.sse"`)
	assert.Contains(t, combined, "firing")
	assert.Contains(t, combined, "afterbegin:#logs-tbody\"")
	assert.Contains(t, combined, "afterbegin:#logs-tbody-test.sse\"")
}

func TestEventsPushesPolicyRemovedOnUnregister(t *testing.T) {
	s, e, r := newTestServer(t)
	registerManualTrigger(t, r, "trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.gone",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	server := httptest.NewServer(s.Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/events")
	require.NoError(t, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)

	require.NoError(t, e.Unregister("test.gone"))

	event := readSSEEvent(t, reader)
	assert.Contains(t, event, `id="policy-status-test.gone"`)
	assert.Contains(t, event, "unregistered")
}
