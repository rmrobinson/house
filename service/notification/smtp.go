package notification

import (
	"bytes"
	"context"
	"fmt"
	"net/smtp"
	"strings"
)

// SMTPSender implements Sender for the "email" channel type, relaying
// through a fixed SMTP host with no authentication - an internal relay
// trusted by source network, not a public mail provider that would need
// credentials.
type SMTPSender struct {
	// Addr is the relay's host:port, e.g. "mail1.ktc1.net.faltung.ca:25".
	Addr string
	// From is the envelope and header From address, e.g.
	// "house@ktc1.net.faltung.ca".
	From string
}

// Send implements Sender.
func (s *SMTPSender) Send(ctx context.Context, to, subject, body, contentType string) error {
	msg := buildMessage(s.From, to, subject, body, contentType)
	if err := smtp.SendMail(s.Addr, nil, s.From, []string{to}, msg); err != nil {
		return fmt.Errorf("notification: sending email to %q via %q: %w", to, s.Addr, err)
	}
	return nil
}

// buildMessage renders a minimal RFC 5322 message with a single
// Content-Type part - no multipart/MIME structure, since every body this
// service is asked to send is either plain text or one self-contained HTML
// document (e.g. the battery-report policy script builds its own HTML
// string entirely in Lua before calling notify.send).
func buildMessage(from, to, subject, body, contentType string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: %s; charset=utf-8\r\n", contentType)
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return b.Bytes()
}
