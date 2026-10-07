package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// writeTestConfig writes a minimal adminui.yaml PersistValues can read and
// patch, and returns its path.
func writeTestConfig(t *testing.T, houseAddr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "adminui.yaml")
	content := "adminui:\n  house_addr: \"" + houseAddr + "\"\n  listen_port: 8080\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// newTestApp builds a real *app against addresses nothing listens on -
// grpcutil.Dial (grpc.Dial) is lazy, so this succeeds without needing a real
// housed/bridge facade behind it, same as adminui.house_addr pointing at a
// momentarily-down housed at real startup.
func newTestApp(t *testing.T) (*app, string) {
	t.Helper()
	configPath := writeTestConfig(t, "127.0.0.1:1")
	a, err := newApp(context.Background(), zaptest.NewLogger(t), configPath, nil, endpoints{HouseAddr: "127.0.0.1:1"}, "")
	require.NoError(t, err)
	return a, configPath
}

func TestHandleSettingsSaveRejectsEmptyHouseAddr(t *testing.T) {
	a, _ := newTestApp(t)
	before := a.current.Load()

	form := "house_addr=&bridge_facade_addr=&policy_addr="
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	a.handleSettingsSave(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "house address is required")
	assert.Same(t, before, a.current.Load(), "a rejected save must not swap the live generation")
}

func TestHandleSettingsSaveRebuildsAndPersists(t *testing.T) {
	a, configPath := newTestApp(t)
	before := a.current.Load()

	form := "house_addr=127.0.0.1%3A2&bridge_facade_addr=&policy_addr=127.0.0.1%3A3"
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	a.handleSettingsSave(rec, req)

	assert.Equal(t, "/settings", rec.Header().Get("HX-Redirect"), "success forces a full page reload so /events reconnects against the new generation")

	after := a.current.Load()
	assert.NotSame(t, before, after, "a successful save must swap in a new generation")
	assert.Equal(t, "127.0.0.1:2", after.endpoints.HouseAddr)
	assert.Equal(t, "127.0.0.1:3", after.endpoints.PolicyAddr)
	assert.True(t, policyConfigured.Load(), "policyAddr was set, so nav should stop greying out Policies")

	saved, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Contains(t, string(saved), "127.0.0.1:2")
	assert.Contains(t, string(saved), "127.0.0.1:3")
}

func TestHandleSettingsGetShowsCurrentEndpoints(t *testing.T) {
	a, _ := newTestApp(t)

	rec := httptest.NewRecorder()
	a.handleSettingsGet(rec, httptest.NewRequest("GET", "/settings", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `value="127.0.0.1:1"`)
}
