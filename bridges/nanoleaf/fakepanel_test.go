package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	nanoleaf "github.com/rmrobinson/nanoleaf-go"

	"github.com/rmrobinson/house/service/bridge"
)

// newFakePanelServer starts a real HTTP+SSE server speaking just enough of the Nanoleaf API for
// panelConn.run's connect/seed/subscribe path: GET "" for GetPanel, PUT "effects" for GetEffects,
// and a single SSE state(1) event on "events" reporting the panel off. This exercises the real
// *nanoleaf.Client (via panelClient's structural typing), not a hand-rolled fake of it - the fake
// in devices_test.go covers command-mapping logic; this covers the wire-level connect/seed path.
func newFakePanelServer(t *testing.T, apiKey string) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/"+apiKey+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(testPanel()))
	})
	mux.HandleFunc("/api/v1/"+apiKey+"/effects", func(w http.ResponseWriter, r *http.Request) {
		var resp struct {
			Effects []nanoleaf.Effect `json:"animations"`
		}
		resp.Effects = testEffects()
		require.NoError(t, json.NewEncoder(w).Encode(resp))
	})
	mux.HandleFunc("/api/v1/"+apiKey+"/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 1\ndata: {\"events\":[{\"attr\":1,\"Value\":false}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})

	return httptest.NewServer(mux)
}

func hostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	host, portStr, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return host, port
}

func TestPanelConn_Run_ConnectSeedAndApplyUpdate(t *testing.T) {
	const apiKey = "test-key"
	srv := newFakePanelServer(t, apiKey)
	defer srv.Close()
	host, port := hostPort(t, srv.URL)

	logger := zaptest.NewLogger(t)
	svc := bridge.NewService(logger)
	nb := NewNanoleafBridge(logger, svc, nil)

	cfg := deviceConfig{ID: "nanoleaf-e2e", Host: host, Port: port, APIKey: apiKey}
	pc := newPanelConn(logger, svc, nb, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		pc.run(ctx)
		close(done)
	}()

	// The panel seeds as on=true (testPanel's default); the fake server's one SSE event flips it
	// to off - waiting for that confirms both the initial seed and the live event stream worked.
	require.Eventually(t, func() bool {
		pc.mu.Lock()
		defer pc.mu.Unlock()
		return pc.device != nil && !pc.device.GetLight().OnOff.State.IsOn
	}, 5*time.Second, 50*time.Millisecond, "state event was never applied")

	nb.mu.Lock()
	_, registered := nb.deviceOwner["nanoleaf-e2e"]
	nb.mu.Unlock()
	assert.True(t, registered, "panelConn should have registered its device with the bridge")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("panelConn.run did not return after ctx was canceled")
	}
}
