package policy

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"
)

// fakeNotifyAPI records every Send call it receives. Several tests
// (ups_water_policies_test.go, battery_report_test.go) trigger a policy
// asynchronously - Engine.trigger runs the script in its own goroutine -
// then poll calls/allCalls from the test goroutine via require.Eventually,
// which itself runs its condition function in a separate goroutine from
// the test body. mu guards against that genuine data race between Send's
// writer and those readers; err is set once before any goroutine starts
// in every test that uses it, so it doesn't need the same protection.
type fakeNotifyAPI struct {
	mu    sync.Mutex
	calls []notifySendCall
	err   error
}

type notifySendCall struct {
	recipientIDs           []string
	subject, body, content string
}

func (f *fakeNotifyAPI) Send(recipientIDs []string, subject, body, contentType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, notifySendCall{recipientIDs, subject, body, contentType})
	return f.err
}

// allCalls returns a snapshot of every Send call recorded so far. Every
// test must read through this (or call/callCount below) rather than the
// calls field directly - see the type's comment for why.
func (f *fakeNotifyAPI) allCalls() []notifySendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]notifySendCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeNotifyAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeNotifyAPI) call(i int) notifySendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

func runScriptForTestWithNotify(t *testing.T, n NotifyAPI, script string) error {
	t.Helper()

	L := lua.NewState()
	defer L.Close()

	registerHomeTable(L, newFakeHomeAPI(), func(string) []string { return nil })
	registerNotifyTable(L, n)

	return L.DoString(script)
}

func TestNotifySendRoundTrip(t *testing.T) {
	n := &fakeNotifyAPI{}

	require.NoError(t, runScriptForTestWithNotify(t, n, `
		notify.send({ to = {"r"}, subject = "Battery report", body = "<p>ok</p>", content_type = "text/html" })
	`))

	require.Equal(t, 1, n.callCount())
	got := n.call(0)
	assert.Equal(t, []string{"r"}, got.recipientIDs)
	assert.Equal(t, "Battery report", got.subject)
	assert.Equal(t, "<p>ok</p>", got.body)
	assert.Equal(t, "text/html", got.content)
}

func TestNotifySendMultipleRecipients(t *testing.T) {
	n := &fakeNotifyAPI{}

	require.NoError(t, runScriptForTestWithNotify(t, n, `
		notify.send({ to = {"r", "other"}, subject = "s", body = "b" })
	`))

	require.Equal(t, 1, n.callCount())
	assert.Equal(t, []string{"r", "other"}, n.call(0).recipientIDs)
	assert.Equal(t, "", n.call(0).content, `content_type defaults to "" at the Lua binding - NotifyAPI implementations default it themselves`)
}

func TestNotifySendMissingToIsScriptCatchableError(t *testing.T) {
	n := &fakeNotifyAPI{}

	err := runScriptForTestWithNotify(t, n, `
		local ok, err = pcall(function() notify.send({ subject = "s", body = "b" }) end)
		assert(ok == false)
	`)
	assert.NoError(t, err, "script pcalls the failing binding, so DoString itself should succeed")
	assert.Zero(t, n.callCount())
}

func TestNotifySendPropagatesNotifyAPIError(t *testing.T) {
	n := &fakeNotifyAPI{err: errors.New("notification service unreachable")}

	err := runScriptForTestWithNotify(t, n, `notify.send({ to = {"r"}, subject = "s", body = "b" })`)
	require.Error(t, err)
	bindingErr := asBindingError(err)
	require.NotNil(t, bindingErr)
	assert.ErrorIs(t, bindingErr, n.err)
}

func TestNotifySendWithNilNotifyAPIRaisesNotImplemented(t *testing.T) {
	err := runScriptForTestWithNotify(t, nil, `notify.send({ to = {"r"}, subject = "s", body = "b" })`)
	require.Error(t, err)
	bindingErr := asBindingError(err)
	require.NotNil(t, bindingErr)
	assert.ErrorIs(t, bindingErr, ErrNotImplemented)
}
