package notification

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
)

// Sender delivers one message over one channel. to is the channel-specific
// destination (a Channel's own Address) - never passed in by a SendMessage
// caller, only ever resolved internally via a Directory lookup.
type Sender interface {
	Send(ctx context.Context, to, subject, body, contentType string) error
}

// Service implements api2.NotificationServiceServer, resolving each
// SendMessageRequest's recipient_ids through directory and fanning out to
// every one of that recipient's channels via senders (keyed by Channel.Type).
type Service struct {
	api2.UnimplementedNotificationServiceServer

	logger    *zap.Logger
	directory *Directory
	senders   map[string]Sender
}

// NewService constructs a Service. senders is keyed by channel type (e.g.
// "email") - a recipient with a channel type that has no entry here is
// skipped for that channel, logged, same as an unknown recipient ID.
func NewService(logger *zap.Logger, directory *Directory, senders map[string]Sender) *Service {
	return &Service{logger: logger, directory: directory, senders: senders}
}

// SendMessage implements api2.NotificationServiceServer. An unknown
// recipient ID, a recipient with no channels configured, or a channel type
// with no registered Sender is logged and skipped rather than failing the
// whole request - one bad entry in a batch shouldn't sink the rest.
// SendMessage only returns an error if nothing was actually attempted (bad
// input) or every attempt failed (the caller otherwise has no signal that
// delivery didn't happen at all).
func (s *Service) SendMessage(ctx context.Context, req *api2.SendMessageRequest) (*api2.SendMessageResponse, error) {
	if len(req.GetRecipientIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "recipient_ids is required")
	}

	contentType := req.GetContentType()
	if contentType == "" {
		contentType = "text/plain"
	}

	var attempted, sent int
	var lastErr error

	for _, id := range req.GetRecipientIds() {
		recipient, ok := s.directory.Lookup(id)
		if !ok {
			s.logger.Warn("unknown recipient, skipping", zap.String("recipient_id", id))
			continue
		}
		if len(recipient.Channels) == 0 {
			s.logger.Warn("recipient has no channels configured, skipping", zap.String("recipient_id", id))
			continue
		}

		for _, ch := range recipient.Channels {
			sender, ok := s.senders[ch.Type]
			if !ok {
				s.logger.Warn("no sender registered for channel type, skipping",
					zap.String("recipient_id", id), zap.String("channel_type", ch.Type))
				continue
			}

			attempted++
			if err := sender.Send(ctx, ch.Address, req.GetSubject(), req.GetBody(), contentType); err != nil {
				s.logger.Error("unable to send message",
					zap.String("recipient_id", id), zap.String("channel_type", ch.Type), zap.Error(err))
				lastErr = err
				continue
			}
			sent++
		}
	}

	switch {
	case attempted == 0:
		return nil, status.Error(codes.InvalidArgument, "no recipient in recipient_ids resolved to a deliverable channel")
	case sent == 0:
		return nil, status.Errorf(codes.Internal, "every delivery attempt failed: %v", lastErr)
	}
	return &api2.SendMessageResponse{}, nil
}
