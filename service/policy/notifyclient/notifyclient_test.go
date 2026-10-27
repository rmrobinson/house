package notifyclient

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/policy"
)

// fakeNotificationServiceClient records the last SendMessageRequest it
// received and returns a configured error, standing in for a real gRPC
// connection to notificationd.
type fakeNotificationServiceClient struct {
	api2.NotificationServiceClient
	lastReq *api2.SendMessageRequest
	err     error
}

func (f *fakeNotificationServiceClient) SendMessage(ctx context.Context, req *api2.SendMessageRequest, opts ...grpc.CallOption) (*api2.SendMessageResponse, error) {
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	return &api2.SendMessageResponse{}, nil
}

func TestSendBuildsSendMessageRequest(t *testing.T) {
	fake := &fakeNotificationServiceClient{}
	c := New(fake)

	require.NoError(t, c.Send([]string{"r", "other"}, policy.NotifyMessage{
		Subject: "subject", Body: "body", ContentType: "text/html",
		Attachments: []policy.NotifyAttachment{{Filename: "a.jpg", ContentType: "image/jpeg", Data: []byte("x"), ContentID: "c1"}},
	}))

	require.NotNil(t, fake.lastReq)
	assert.Equal(t, []string{"r", "other"}, fake.lastReq.GetRecipientIds())
	assert.Equal(t, "subject", fake.lastReq.GetSubject())
	assert.Equal(t, "body", fake.lastReq.GetBody())
	assert.Equal(t, "text/html", fake.lastReq.GetContentType())
	require.Len(t, fake.lastReq.GetAttachments(), 1)
	assert.Equal(t, "c1", fake.lastReq.GetAttachments()[0].GetContentId())
	assert.Equal(t, []byte("x"), fake.lastReq.GetAttachments()[0].GetData())
}

func TestSendPropagatesRPCError(t *testing.T) {
	fake := &fakeNotificationServiceClient{err: errors.New("unavailable")}
	c := New(fake)

	err := c.Send([]string{"r"}, policy.NotifyMessage{Subject: "s", Body: "b"})
	require.Error(t, err)
	assert.ErrorIs(t, err, fake.err)
}
