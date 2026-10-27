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
	attachments                    []*api2.Attachment
}

func (f *fakeSender) Send(ctx context.Context, to, subject, body, contentType string, attachments []*api2.Attachment) error {
	f.calls = append(f.calls, sendCall{to, subject, body, contentType, attachments})
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
	assert.Equal(t, sendCall{"r@example.com", "subject", "body", "text/html", nil}, email.calls[0])
	require.Len(t, push.calls, 1)
	assert.Equal(t, sendCall{"token-123", "subject", "body", "text/html", nil}, push.calls[0])
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

func attachmentService(t *testing.T) (*Service, *fakeSender) {
	email := &fakeSender{}
	directory := NewDirectory([]Recipient{{ID: "r", Channels: []Channel{{Type: "email", Address: "r@example.com"}}}})
	return newTestService(t, directory, map[string]Sender{"email": email}), email
}

func TestSendMessagePassesAttachmentsThrough(t *testing.T) {
	svc, email := attachmentService(t)
	att := &api2.Attachment{Filename: "a.jpg", ContentType: "image/jpeg", Data: []byte("x"), ContentId: "cam1"}

	_, err := svc.SendMessage(context.Background(), &api2.SendMessageRequest{
		RecipientIds: []string{"r"}, Subject: "s", Body: "b", Attachments: []*api2.Attachment{att},
	})
	require.NoError(t, err)
	require.Len(t, email.calls, 1)
	assert.Equal(t, []*api2.Attachment{att}, email.calls[0].attachments)
}

func TestSendMessageRejectsBadAttachments(t *testing.T) {
	good := func() *api2.Attachment {
		return &api2.Attachment{Filename: "a.jpg", ContentType: "image/jpeg", Data: []byte("x"), ContentId: "cam1"}
	}
	cases := map[string][]*api2.Attachment{
		"empty data":        {{ContentType: "image/jpeg"}},
		"bad content type":  {{ContentType: "image/jpeg\r\nBcc: x@y", Data: []byte("x")}},
		"no content type":   {{Data: []byte("x")}},
		"bad content id":    {{ContentType: "image/jpeg", Data: []byte("x"), ContentId: "a b>\r\n"}},
		"duplicate id":      {good(), good()},
		"over total budget": {{ContentType: "image/jpeg", Data: make([]byte, MaxAttachmentBytes/2+1)}, {ContentType: "image/jpeg", Data: make([]byte, MaxAttachmentBytes/2+1)}},
	}
	for name, atts := range cases {
		t.Run(name, func(t *testing.T) {
			svc, email := attachmentService(t)
			_, err := svc.SendMessage(context.Background(), &api2.SendMessageRequest{RecipientIds: []string{"r"}, Attachments: atts})
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Empty(t, email.calls)
		})
	}
}
