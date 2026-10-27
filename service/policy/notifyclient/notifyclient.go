// Package notifyclient implements policy.NotifyAPI over a gRPC connection
// to a running notificationd (see service/notification). It is deliberately
// tiny compared to bridgehome.Adapter: NotificationService.SendMessage is a
// single stateless RPC, so there's no cache or stream to maintain here.
package notifyclient

import (
	"context"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/policy"
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
func (c *Client) Send(recipientIDs []string, msg policy.NotifyMessage) error {
	req := &api2.SendMessageRequest{
		RecipientIds: recipientIDs,
		Subject:      msg.Subject,
		Body:         msg.Body,
		ContentType:  msg.ContentType,
	}
	for _, a := range msg.Attachments {
		req.Attachments = append(req.Attachments, &api2.Attachment{
			Filename:    a.Filename,
			ContentType: a.ContentType,
			Data:        a.Data,
			ContentId:   a.ContentID,
		})
	}

	_, err := c.client.SendMessage(context.Background(), req)
	return err
}
