// Package notification implements api.NotificationServiceServer: a small
// gRPC service that lets a caller (the policy engine, via
// service/policy/notifyclient) send a message to a person by recipient ID,
// with no knowledge of how that person is actually reached. This package
// owns the recipient-ID-to-channel mapping and every channel type that
// exists - see Directory and Sender.
package notification

// Channel is one way to reach a Recipient. Type selects which Sender (see
// Service.senders) handles delivery; Address is that Sender's own
// destination string - an email address for Type "email" today, a push
// token for a future channel type.
type Channel struct {
	Type    string
	Address string
}

// Recipient is a person the notification service can reach, resolved by ID
// from a SendMessageRequest. The caller never sees Channels directly -
// NotificationService.SendMessage fans out to every one of them.
type Recipient struct {
	ID       string
	Name     string
	Channels []Channel
}

// Directory resolves a recipient ID to the Recipient it names. Backed by a
// static, startup-loaded list for now (see cmd/notificationd/main.go,
// which populates one from notification.recipients in its YAML config) -
// Service only ever calls Lookup, so a store-backed Directory later (for
// managing recipients without a redeploy) is a drop-in replacement with no
// change to Service or the wire API.
type Directory struct {
	byID map[string]Recipient
}

// NewDirectory builds a Directory from recipients, keyed by their own ID.
// A duplicate ID keeps whichever entry appears last in recipients.
func NewDirectory(recipients []Recipient) *Directory {
	byID := make(map[string]Recipient, len(recipients))
	for _, r := range recipients {
		byID[r.ID] = r
	}
	return &Directory{byID: byID}
}

// Lookup returns the Recipient registered under id, and whether one was
// found.
func (d *Directory) Lookup(id string) (Recipient, bool) {
	r, ok := d.byID[id]
	return r, ok
}
