package notification

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api2 "github.com/rmrobinson/house/api"
)

func TestBuildMessagePlainText(t *testing.T) {
	msg := string(buildMessage("house@example.com", "r@example.com", "subject", "line one\nline two", "text/plain"))

	assert.Contains(t, msg, "From: house@example.com\r\n")
	assert.Contains(t, msg, "To: r@example.com\r\n")
	assert.Contains(t, msg, "Subject: subject\r\n")
	assert.Contains(t, msg, "Content-Type: text/plain; charset=utf-8\r\n")
	assert.Contains(t, msg, "line one\r\nline two")

	headerEnd := strings.Index(msg, "\r\n\r\n")
	assert.Greater(t, headerEnd, 0, "message must separate headers from body with a blank line")
}

func TestBuildMessageHTML(t *testing.T) {
	msg := string(buildMessage("house@example.com", "r@example.com", "Battery report", "<html><body>ok</body></html>", "text/html"))

	assert.Contains(t, msg, "Content-Type: text/html; charset=utf-8\r\n")
	assert.Contains(t, msg, "<html><body>ok</body></html>")
}

// bodyOf returns the message body (everything after the blank line) decoded
// the way a mail client would: through the declared Content-Transfer-Encoding.
func bodyOf(t *testing.T, msg []byte) string {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(msg))
	require.NoError(t, err)
	var r io.Reader = m.Body
	if m.Header.Get("Content-Transfer-Encoding") == "quoted-printable" {
		r = quotedprintable.NewReader(m.Body)
	}
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(b)
}

// TestBuildMessageLongSingleLineBodyRespectsLineLimit is the regression test
// for the battery-report email whose HTML (built as one 1,258-character line
// by the Lua script) rendered a literal "< li>" in one list item: SMTP caps
// lines at 998 characters, the relay broke the line there, and the break
// landed right after a "<" - a "<" followed by whitespace is text, not a tag.
// The body below puts the "<" of an <li> at exactly character 998.
func TestBuildMessageLongSingleLineBodyRespectsLineLimit(t *testing.T) {
	body := "<html><body><ul>" + strings.Repeat("a", 997-len("<html><body><ul>")) + "<li>tail</li></ul></body></html>"
	require.Equal(t, byte('<'), body[997], "the < of the <li> must be the 998th character")
	require.NotContains(t, body, "\n", "the body must be a single line, like the Lua-built HTML")

	msg := buildMessage("house@example.com", "r@example.com", "Battery report", body, "text/html")

	_, encoded, found := strings.Cut(string(msg), "\r\n\r\n")
	require.True(t, found)
	for _, line := range strings.Split(encoded, "\r\n") {
		assert.LessOrEqual(t, len(line), 76, "transfer-encoded line too long for any relay to leave alone: %q", line)
	}
	assert.Equal(t, body, bodyOf(t, msg), "a mail client must decode back exactly the original HTML")
}

func TestBuildMessageRoundTripsNonASCIIAndNewlines(t *testing.T) {
	for _, body := range []string{
		"café — 100% ✓ and a = sign",
		"line one\nline two\n\nline four",
		"trailing spaces   \nand a tab\t\nend",
		"",
	} {
		msg := buildMessage("house@example.com", "r@example.com", "s", body, "text/plain")
		assert.Equal(t, strings.ReplaceAll(body, "\n", "\r\n"), bodyOf(t, msg), "body %q", body)
	}
}

func TestBuildMessageStripsHeaderInjection(t *testing.T) {
	msg := string(buildMessage(
		"house@example.com",
		"r@example.com\r\nBcc: attacker@evil.com",
		"subject\r\nBcc: attacker@evil.com",
		"body",
		"text/plain",
	))

	header, _, found := strings.Cut(msg, "\r\n\r\n")
	require.True(t, found, "message must separate headers from body with a blank line")

	lines := strings.Split(header, "\r\n")
	for _, line := range lines {
		assert.False(t, strings.HasPrefix(line, "Bcc:"), "a CRLF in subject/to must not be able to inject a new header line, got %q", line)
	}
	assert.Len(t, lines, 8, "CRLF in subject/to must be stripped rather than producing extra header lines")
}

func TestBuildMessageHasDateAndMessageID(t *testing.T) {
	msg := string(buildMessage("house@example.com", "r@example.com", "subject", "body", "text/plain"))

	assert.Regexp(t, `\r\nDate: \S.*\r\n`, msg, "a message missing Date is a strong spam signal to downstream filters")
	assert.Regexp(t, `\r\nMessage-Id: <\S+@example\.com>\r\n`, msg, "a message missing Message-Id is a strong spam signal to downstream filters")
}

func parseMultipart(t *testing.T, raw []byte) (*mail.Message, *multipart.Reader) {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(mt, "multipart/"), mt)
	return m, multipart.NewReader(m.Body, params["boundary"])
}

func TestBuildMessageNoAttachmentsIsUnchanged(t *testing.T) {
	// The attachment-aware builder must be byte-identical for the common case apart from the volatile
	// Date and Message-Id headers.
	strip := func(b []byte) string {
		var out []string
		for _, l := range strings.Split(string(b), "\r\n") {
			if strings.HasPrefix(l, "Date:") || strings.HasPrefix(l, "Message-Id:") {
				continue
			}
			out = append(out, l)
		}
		return strings.Join(out, "\r\n")
	}
	a := buildMessage("house@example.com", "r@example.com", "s", "<b>hi</b>", "text/html")
	b := buildMessageWithAttachments("house@example.com", "r@example.com", "s", "<b>hi</b>", "text/html", nil)
	assert.Equal(t, strip(a), strip(b))
}

func TestBuildMessageWithInlineImage(t *testing.T) {
	img := make([]byte, 1000)
	for i := range img {
		img[i] = byte(i)
	}
	body := "<html><body><img src=\"cid:cam1\">" + strings.Repeat("x", 2000) + "</body></html>"
	raw := buildMessageWithAttachments("house@example.com", "r@example.com", "Motion", body, "text/html", []*api2.Attachment{
		{Filename: "back\"yard.jpg", ContentType: "image/jpeg", Data: img, ContentId: "cam1"},
		{Filename: "plain.txt", ContentType: "text/plain", Data: []byte("note")},
	})

	m, mr := parseMultipart(t, raw)
	mt, _, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	assert.Equal(t, "multipart/related", mt)

	for _, line := range strings.Split(string(raw), "\r\n") {
		assert.LessOrEqual(t, len(line), 998)
	}

	p, err := mr.NextRawPart()
	require.NoError(t, err)
	assert.Equal(t, "quoted-printable", p.Header.Get("Content-Transfer-Encoding"))
	text, err := io.ReadAll(quotedprintable.NewReader(p))
	require.NoError(t, err)
	assert.Equal(t, body, string(text))

	p, err = mr.NextRawPart()
	require.NoError(t, err)
	assert.Equal(t, "<cam1>", p.Header.Get("Content-Id"))
	assert.Contains(t, p.Header.Get("Content-Disposition"), "inline")
	assert.NotContains(t, p.Header.Get("Content-Disposition"), "back\"yard")
	enc, err := io.ReadAll(p)
	require.NoError(t, err)
	for _, line := range strings.Split(strings.TrimRight(string(enc), "\r\n"), "\r\n") {
		assert.LessOrEqual(t, len(line), 76)
	}
	dec, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(enc), "\r\n", ""))
	require.NoError(t, err)
	assert.Equal(t, img, dec)

	p, err = mr.NextRawPart()
	require.NoError(t, err)
	assert.Empty(t, p.Header.Get("Content-Id"))
	assert.Contains(t, p.Header.Get("Content-Disposition"), "attachment")

	_, err = mr.NextRawPart()
	assert.Equal(t, io.EOF, err)
}

func TestBuildMessagePlainTextWithAttachmentIsMixed(t *testing.T) {
	raw := buildMessageWithAttachments("house@example.com", "r@example.com", "s", "hi", "text/plain",
		[]*api2.Attachment{{ContentType: "image/jpeg", Data: []byte("x")}})
	m, _ := parseMultipart(t, raw)
	mt, _, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	assert.Equal(t, "multipart/mixed", mt)
	assert.Contains(t, string(raw), `filename="attachment"`)
}

func TestBuildMessageAttachmentHeadersCannotInject(t *testing.T) {
	raw := buildMessageWithAttachments("house@example.com", "r@example.com", "s", "hi", "text/html",
		[]*api2.Attachment{{Filename: "a\r\nBcc: evil@example.com", ContentType: "image/jpeg", Data: []byte("x"), ContentId: "id\r\nBcc: evil@example.com"}})
	assert.NotContains(t, string(raw), "\r\nBcc:")
}
