package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPWAAssetsAreServed(t *testing.T) {
	s, _, _ := startTestServer(t)

	m := get(s, "/manifest.webmanifest")
	assert.Equal(t, http.StatusOK, m.Code)
	assert.Equal(t, "application/manifest+json", m.Header().Get("Content-Type"))
	var manifest struct {
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
		Display  string `json:"display"`
		Icons    []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	require.NoError(t, json.Unmarshal(m.Body.Bytes(), &manifest))
	assert.Equal(t, "standalone", manifest.Display)
	assert.Equal(t, "/", manifest.StartURL)
	assert.Equal(t, "/", manifest.Scope)
	require.NotEmpty(t, manifest.Icons)
	for _, ic := range manifest.Icons {
		rec := get(s, ic.Src)
		assert.Equal(t, http.StatusOK, rec.Code, ic.Src)
		assert.Equal(t, "image/png", rec.Header().Get("Content-Type"), ic.Src)
	}

	sw := get(s, "/sw.js")
	assert.Equal(t, http.StatusOK, sw.Code)
	assert.Contains(t, sw.Header().Get("Content-Type"), "javascript")
	assert.Equal(t, "no-cache", sw.Header().Get("Cache-Control"))
	assert.Equal(t, "DENY", sw.Header().Get("X-Frame-Options"))

	off := get(s, "/offline")
	assert.Equal(t, http.StatusOK, off.Code)
	assert.Contains(t, off.Body.String(), "CAN'T REACH HOUSE")

	assert.Equal(t, http.StatusOK, get(s, "/static/icons/apple-touch-icon.png").Code)
	assert.Equal(t, http.StatusNotFound, get(s, "/static/icons/nope.png").Code)
	assert.Equal(t, http.StatusNotFound, get(s, "/static/icons/..%2fmanifest.webmanifest").Code)
}

func TestPagesLinkTheManifest(t *testing.T) {
	s, _, _ := startTestServer(t)
	body := get(s, "/buildings/b1").Body.String()
	assert.Contains(t, body, `<link rel="manifest" href="/manifest.webmanifest">`)
	assert.Contains(t, body, `rel="apple-touch-icon"`)
	assert.Contains(t, body, "viewport-fit=cover")
}
