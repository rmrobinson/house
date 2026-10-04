// Package notifyclient implements policy.NotifyAPI over a gRPC connection
// to a running notificationd (see service/notification). It is deliberately
// tiny compared to bridgehome.Adapter: NotificationService.SendMessage is a
// single stateless RPC, so there's no cache or stream to maintain here.
package notifyclient

import (
	"context"

	api2 "github.com/rmrobinson/house/api"
)

// Client implements policy.NotifyAPI by calling NotificationServiceClient.
// SendMessage.
type Client struct {
	client api2.NotificationServiceClient
}

// New wraps client as a policy.NotifyAPI.
func New(client api2.NotificationServiceClient) *Client {
	return &Client{client: client}
}

// Send implements policy.NotifyAPI.
func (c *Client) Send(recipientIDs []string, subject, body, contentType string) error {
	_, err := c.client.SendMessage(context.Background(), &api2.SendMessageRequest{
		RecipientIds: recipientIDs,
		Subject:      subject,
		Body:         body,
		ContentType:  contentType,
	})
	return err
}
