package notification

import (
	"bytes"
	"io"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
