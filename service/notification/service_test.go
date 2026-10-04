package notification

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
)

// fakeSender records every call it receives and returns a per-"to" error
// if one is configured via errs.
type fakeSender struct {
	calls []sendCall
	errs  map[string]error
}

type sendCall struct {
	to, subject, body, contentType string
}

func (f *fakeSender) Send(ctx context.Context, to, subject, body, contentType string) error {
	f.calls = append(f.calls, sendCall{to, subject, body, contentType})
	return f.errs[to]
}

func newTestService(t *testing.T, directory *Directory, senders map[string]Sender) *Service {
	return NewService(zaptest.NewLogger(t), directory, senders)
}

func TestSendMessageFansOutToEveryChannel(t *testing.T) {
	directory := NewDirectory([]Recipient{
		{ID: "r", Channels: []Channel{
			{Type: "email", Address: "r@example.com"},
			{Type: "push", Address: "token-123"},
		}},
	})
	email := &fakeSender{}
	push := &fakeSender{}
	svc := newTestService(t, directory, map[string]Sender{"email": email, "push": push})

	_, err := svc.SendMessage(context.Background(), &api2.SendMessageRequest{
		RecipientIds: []string{"r"},
		Subject:      "subject",
		Body:         "body",
		ContentType:  "text/html",
	})
	require.NoError(t, err)

	require.Len(t, email.calls, 1)
	assert.Equal(t, sendCall{"r@example.com", "subject", "body", "text/html"}, email.calls[0])
	require.Len(t, push.calls, 1)
	assert.Equal(t, sendCall{"token-123", "subject", "body", "text/html"}, push.calls[0])
}

func TestSendMessageDefaultsContentTypeToPlain(t *testing.T) {
	directory := NewDirectory([]Recipient{
		{ID: "r", Channels: []Channel{{Type: "email", Address: "r@example.com"}}},
	})
	email := &fakeSender{}
	svc := newTestService(t, directory, map[string]Sender{"email": email})

	_, err := svc.SendMessage(context.Background(), &api2.SendMessageRequest{RecipientIds: []string{"r"}})
	require.NoError(t, err)

	require.Len(t, email.calls, 1)
	assert.Equal(t, "text/plain", email.calls[0].contentType)
}

func TestSendMessageUnknownRecipientIsSkippedNotFatal(t *testing.T) {
	directory := NewDirectory([]Recipient{
		{ID: "r", Channels: []Channel{{Type: "email", Address: "r@example.com"}}},
	})
	email := &fakeSender{}
	svc := newTestService(t, directory, map[string]Sender{"email": email})

	_, err := svc.SendMessage(context.Background(), &api2.SendMessageRequest{
		RecipientIds: []string{"nobody", "r"},
	})
	require.NoError(t, err)
	assert.Len(t, email.calls, 1)
}

func TestSendMessageNoDeliverableRecipientsIsInvalidArgument(t *testing.T) {
	directory := NewDirectory([]Recipient{
		{ID: "nochannels", Channels: nil},
	})
	svc := newTestService(t, directory, map[string]Sender{})

	_, err := svc.SendMessage(context.Background(), &api2.SendMessageRequest{
		RecipientIds: []string{"nobody", "nochannels"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestSendMessageEmptyRecipientIdsIsInvalidArgument(t *testing.T) {
	svc := newTestService(t, NewDirectory(nil), map[string]Sender{})

	_, err := svc.SendMessage(context.Background(), &api2.SendMessageRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestSendMessageOnePartialFailureStillSucceeds(t *testing.T) {
	directory := NewDirectory([]Recipient{
		{ID: "r", Channels: []Channel{
			{Type: "email", Address: "good@example.com"},
			{Type: "push", Address: "bad-token"},
		}},
	})
	email := &fakeSender{}
	push := &fakeSender{errs: map[string]error{"bad-token": errors.New("push failed")}}
	svc := newTestService(t, directory, map[string]Sender{"email": email, "push": push})

	_, err := svc.SendMessage(context.Background(), &api2.SendMessageRequest{RecipientIds: []string{"r"}})
	require.NoError(t, err)
}

func TestSendMessageEveryAttemptFailingIsInternalError(t *testing.T) {
	directory := NewDirectory([]Recipient{
		{ID: "r", Channels: []Channel{{Type: "email", Address: "r@example.com"}}},
	})
	email := &fakeSender{errs: map[string]error{"r@example.com": errors.New("smtp down")}}
	svc := newTestService(t, directory, map[string]Sender{"email": email})

	_, err := svc.SendMessage(context.Background(), &api2.SendMessageRequest{RecipientIds: []string{"r"}})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}
