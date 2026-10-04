package notification

import (
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
	assert.Len(t, lines, 7, "CRLF in subject/to must be stripped rather than producing extra header lines")
}

func TestBuildMessageHasDateAndMessageID(t *testing.T) {
	msg := string(buildMessage("house@example.com", "r@example.com", "subject", "body", "text/plain"))

	assert.Regexp(t, `\r\nDate: \S.*\r\n`, msg, "a message missing Date is a strong spam signal to downstream filters")
	assert.Regexp(t, `\r\nMessage-Id: <\S+@example\.com>\r\n`, msg, "a message missing Message-Id is a strong spam signal to downstream filters")
}
