package policy

// NotifyAttachment is a file sent with a notification.
type NotifyAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
	// ContentID, if set, lets an HTML body show the file inline with <img src="cid:...">.
	ContentID string
}

// NotifyMessage is what a policy script asks to be delivered.
type NotifyMessage struct {
	Subject string
	Body    string
	// ContentType is Body's MIME type (e.g. "text/html", "text/plain"); empty leaves the choice to
	// whatever's behind the NotifyAPI (service/notification treats it as "text/plain").
	ContentType string
	Attachments []NotifyAttachment
}

// NotifyAPI is the surface a policy script uses to reach a person via an
// external channel (email today; FCM push and others later) - deliberately
// separate from HomeAPI: HomeAPI reads/writes the house's own device/state
// model, NotifyAPI reaches someone outside it. recipientIDs name people,
// never addresses or channel types; the notification service behind a
// concrete NotifyAPI implementation (see service/policy/notifyclient) owns
// that id-to-channel mapping entirely.
type NotifyAPI interface {
	// Send delivers msg to every one of recipientIDs.
	Send(recipientIDs []string, msg NotifyMessage) error
}
