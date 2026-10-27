package notification

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"mime/quotedprintable"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	api2 "github.com/rmrobinson/house/api"
)

// SMTPSender implements Sender for the "email" channel type, relaying
// through a fixed SMTP host with no authentication - an internal relay
// trusted by source network, not a public mail provider that would need
// credentials.
type SMTPSender struct {
	// Addr is the relay's host:port, e.g. "mail.example.com:25".
	Addr string
	// From is the envelope and header From address, e.g.
	// "house@example.com".
	From string
}

// Send implements Sender.
func (s *SMTPSender) Send(ctx context.Context, to, subject, body, contentType string, attachments []*api2.Attachment) error {
	to = stripCRLF(to)
	msg := buildMessageWithAttachments(s.From, to, subject, body, contentType, attachments)
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
//
// The body is quoted-printable encoded. That script builds its HTML as one
// line with no newlines, and SMTP caps a line at 998 characters (RFC 5321
// section 4.5.3.1.6): a relay breaks anything longer at that column, and when
// the break lands just after a "<" the tag turns into literal "< li>" text in
// the recipient's mail client. Quoted-printable wraps lines at 76 characters
// with soft breaks (an "=" at the end of a line, removed on decoding) that a
// mail client never shows, and it also carries non-ASCII text, which an
// unencoded "charset=utf-8" body with no declared 8-bit transport would not.
func buildMessage(from, to, subject, body, contentType string) []byte {
	return buildMessageWithAttachments(from, to, subject, body, contentType, nil)
}

// buildMessageWithAttachments is buildMessage plus files. With none the output is exactly buildMessage's
// single-part message. With some, the body becomes the first part of a multipart message - multipart/related
// for an HTML body, so cid: references resolve, otherwise multipart/mixed - and each attachment is a base64
// part wrapped at 76 characters. The body part keeps its quoted-printable encoding and so the line-length
// guarantee above.
func buildMessageWithAttachments(from, to, subject, body, contentType string, attachments []*api2.Attachment) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", stripCRLF(from))
	fmt.Fprintf(&b, "To: %s\r\n", stripCRLF(to))
	fmt.Fprintf(&b, "Subject: %s\r\n", stripCRLF(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-Id: %s\r\n", newMessageID(from))
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")

	if len(attachments) == 0 {
		fmt.Fprintf(&b, "Content-Type: %s; charset=utf-8\r\n", stripCRLF(contentType))
		fmt.Fprintf(&b, "Content-Transfer-Encoding: quoted-printable\r\n")
		b.WriteString("\r\n")
		writeQuotedPrintable(&b, body)
		return b.Bytes()
	}

	subtype := "mixed"
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/html") {
		subtype = "related"
	}
	mw := multipart.NewWriter(&b)
	fmt.Fprintf(&b, "Content-Type: multipart/%s; boundary=\"%s\"\r\n\r\n", subtype, mw.Boundary())

	// Writing to a bytes.Buffer cannot fail.
	bodyPart, _ := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {stripCRLF(contentType) + "; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	writeQuotedPrintable(bodyPart, body)

	for _, a := range attachments {
		name := attachmentFilename(a.GetFilename())
		h := textproto.MIMEHeader{
			"Content-Type":              {fmt.Sprintf("%s; name=\"%s\"", stripCRLF(a.GetContentType()), name)},
			"Content-Transfer-Encoding": {"base64"},
		}
		if id := stripCRLF(a.GetContentId()); id != "" {
			h.Set("Content-Id", "<"+id+">")
			h.Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", name))
		} else {
			h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", name))
		}
		part, _ := mw.CreatePart(h)
		writeBase64Wrapped(part, a.GetData())
	}
	_ = mw.Close()
	return b.Bytes()
}

// writeQuotedPrintable writes body quoted-printable encoded. In its default (text) mode the writer turns
// each line break into CRLF itself, so body is passed through unmodified.
func writeQuotedPrintable(w io.Writer, body string) {
	qp := quotedprintable.NewWriter(w)
	_, _ = qp.Write([]byte(body))
	_ = qp.Close()
}

// writeBase64Wrapped writes data base64 encoded in CRLF-terminated lines of 76 characters.
func writeBase64Wrapped(w io.Writer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 0 {
		n := min(76, len(enc))
		_, _ = io.WriteString(w, enc[:n]+"\r\n")
		enc = enc[n:]
	}
}

// attachmentFilename makes a display name safe to place in a quoted MIME header parameter.
func attachmentFilename(name string) string {
	name = strings.NewReplacer("\r", "", "\n", "", "\"", "", "\\", "").Replace(name)
	if name == "" {
		return "attachment"
	}
	return name
}

// stripCRLF removes carriage returns and line feeds so a value can't
// terminate a header early and inject additional headers.
func stripCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	return strings.ReplaceAll(s, "\n", "")
}

// newMessageID generates a unique RFC 5322 Message-Id value (angle brackets
// included), using from's domain as the right-hand side - the same
// convention most MTAs/MUAs follow. Without one, some relays (e.g.
// postfix's cleanup daemon, depending on its own config) send the message
// on with no Message-Id at all rather than synthesizing a replacement,
// which downstream spam filters treat as a strong signal against the
// message, independent of whatever those filters make of its DKIM status.
func newMessageID(from string) string {
	domain := "localhost"
	if i := strings.LastIndex(from, "@"); i >= 0 {
		domain = from[i+1:]
	}

	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("<%d@%s>", time.Now().UnixNano(), domain)
	}
	return fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(buf[:]), domain)
}
