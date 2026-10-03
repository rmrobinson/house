package policy

// NotifyAPI is the surface a policy script uses to reach a person via an
// external channel (email today; FCM push and others later) - deliberately
// separate from HomeAPI: HomeAPI reads/writes the house's own device/state
// model, NotifyAPI reaches someone outside it. recipientIDs name people,
// never addresses or channel types; the notification service behind a
// concrete NotifyAPI implementation (see service/policy/notifyclient) owns
// that id-to-channel mapping entirely.
type NotifyAPI interface {
	// Send delivers subject/body to every one of recipientIDs. contentType
	// is body's MIME type (e.g. "text/html", "text/plain"); an empty
	// contentType leaves the choice to whatever's behind this NotifyAPI
	// (service/notification treats it as "text/plain").
	Send(recipientIDs []string, subject, body, contentType string) error
}
