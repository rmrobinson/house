package policy

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"
)

// fakeNotifyAPI records every Send call it receives.
type fakeNotifyAPI struct {
	calls []notifySendCall
	err   error
}

type notifySendCall struct {
	recipientIDs           []string
	subject, body, content string
}

func (f *fakeNotifyAPI) Send(recipientIDs []string, subject, body, contentType string) error {
	f.calls = append(f.calls, notifySendCall{recipientIDs, subject, body, contentType})
	return f.err
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

	require.Len(t, n.calls, 1)
	got := n.calls[0]
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

	require.Len(t, n.calls, 1)
	assert.Equal(t, []string{"r", "other"}, n.calls[0].recipientIDs)
	assert.Equal(t, "", n.calls[0].content, `content_type defaults to "" at the Lua binding - NotifyAPI implementations default it themselves`)
}

func TestNotifySendMissingToIsScriptCatchableError(t *testing.T) {
	n := &fakeNotifyAPI{}

	err := runScriptForTestWithNotify(t, n, `
		local ok, err = pcall(function() notify.send({ subject = "s", body = "b" }) end)
		assert(ok == false)
	`)
	assert.NoError(t, err, "script pcalls the failing binding, so DoString itself should succeed")
	assert.Empty(t, n.calls)
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
