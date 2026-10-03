package notification

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildMessagePlainText(t *testing.T) {
	msg := string(buildMessage("house@ktc1.net.faltung.ca", "r@faltung.ca", "subject", "line one\nline two", "text/plain"))

	assert.Contains(t, msg, "From: house@ktc1.net.faltung.ca\r\n")
	assert.Contains(t, msg, "To: r@faltung.ca\r\n")
	assert.Contains(t, msg, "Subject: subject\r\n")
	assert.Contains(t, msg, "Content-Type: text/plain; charset=utf-8\r\n")
	assert.Contains(t, msg, "line one\r\nline two")

	headerEnd := strings.Index(msg, "\r\n\r\n")
	assert.Greater(t, headerEnd, 0, "message must separate headers from body with a blank line")
}

func TestBuildMessageHTML(t *testing.T) {
	msg := string(buildMessage("house@ktc1.net.faltung.ca", "r@faltung.ca", "Battery report", "<html><body>ok</body></html>", "text/html"))

	assert.Contains(t, msg, "Content-Type: text/html; charset=utf-8\r\n")
	assert.Contains(t, msg, "<html><body>ok</body></html>")
}
