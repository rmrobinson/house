package main

import (
	"testing"

	"github.com/koron/go-ssdp"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zaptest"
)

func TestUUIDFromUSN(t *testing.T) {
	cases := []struct {
		usn    string
		wantID string
		wantOK bool
	}{
		{"uuid:8996a15f-1234-0000-0000-000000000000::urn:lge-com:service:webos-second-screen:1", "8996a15f-1234-0000-0000-000000000000", true},
		{"uuid:only-uuid-no-service", "", false}, // missing the "::" separator every real USN has
		{"urn:lge-com:service:webos-second-screen:1", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		id, ok := uuidFromUSN(c.usn)
		assert.Equal(t, c.wantOK, ok, c.usn)
		assert.Equal(t, c.wantID, id, c.usn)
	}
}

func TestHostFromLocation(t *testing.T) {
	cases := []struct {
		location string
		wantHost string
		wantOK   bool
	}{
		{"http://192.168.1.42:1900/description.xml", "192.168.1.42", true},
		{"not a url", "", false},
		{"http://:1900/", "", false},
	}
	for _, c := range cases {
		host, ok := hostFromLocation(c.location)
		assert.Equal(t, c.wantOK, ok, c.location)
		assert.Equal(t, c.wantHost, host, c.location)
	}
}

func TestDiscoverDevices_FiltersMalformedEntries(t *testing.T) {
	search := func(searchType string, waitSec int, localAddr string) ([]ssdp.Service, error) {
		assert.Equal(t, searchTarget, searchType)
		return []ssdp.Service{
			{USN: "uuid:good-uuid::urn:lge-com:service:webos-second-screen:1", Location: "http://192.168.1.42:1900/x"},
			{USN: "", Location: "http://192.168.1.43:1900/x"},        // missing uuid
			{USN: "uuid:another-uuid::urn:x", Location: "not-a-url"}, // missing usable location
		}, nil
	}

	found := discoverDevices(zaptest.NewLogger(t), search)
	if assert.Len(t, found, 1) {
		assert.Equal(t, "good-uuid", found[0].UUID)
		assert.Equal(t, "192.168.1.42", found[0].Host)
	}
}
