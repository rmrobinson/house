package notifyclient

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
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

	require.NoError(t, c.Send([]string{"r", "other"}, "subject", "body", "text/html"))

	require.NotNil(t, fake.lastReq)
	assert.Equal(t, []string{"r", "other"}, fake.lastReq.GetRecipientIds())
	assert.Equal(t, "subject", fake.lastReq.GetSubject())
	assert.Equal(t, "body", fake.lastReq.GetBody())
	assert.Equal(t, "text/html", fake.lastReq.GetContentType())
}

func TestSendPropagatesRPCError(t *testing.T) {
	fake := &fakeNotificationServiceClient{err: errors.New("unavailable")}
	c := New(fake)

	err := c.Send([]string{"r"}, "s", "b", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, fake.err)
}
