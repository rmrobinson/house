package main

import (
	"bytes"
	"fmt"
	"net/http"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/htmxutil"
)

// handleSSE relays the shared deviceHub's and policyHub's updates as
// Server-Sent Events for htmx's SSE extension (see templates/layout.html's
// hx-ext="sse") - one connection per open tab carrying both house and
// policy live updates, rather than each owning its own SSE connection.
//
// Every page shows a device's name/kind/online status in a
// <td id="device-info-<id>"> cell (see templates/partials/device_info.html),
// separate from any room-link action cell around it, which is
// page-specific (room page: unlink; devices page: link/move) and untouched
// by a device-state update. A DeviceUpdate is re-emitted as that same cell
// carrying hx-swap-oob, so htmx swaps it wherever it currently exists in the
// DOM - no page-specific subscription bookkeeping needed.
// BridgeUpdate/CommandUpdate/InitialUpdate are not relayed: this admin UI
// has no view of bridge-level or in-flight command state, only device info.
//
// A PolicyEvent is relayed the same way: policy_changed/policy_removed patch
// whichever #policy-status-<id> element is currently on the page (list row,
// detail page, or both), and log_appended prepends a row into both the
// global log feed and that policy's own detail-page feed - ported from
// service/policy/http.go's handleEvents/pushPolicyChanged/pushLogAppended.
//
// One upstream connection per hub (see hub.go/policy_hub.go) backs every
// open browser tab, rather than each tab opening its own - a new tab simply
// subscribes to updates already flowing, so it doesn't pay for (or need)
// another copy of either stream's initial-state replay: the page it just
// loaded already rendered current state itself via its own RPC calls, and a
// push here only ever swaps a cell/row that render already produced.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.httpError(w, r, fmt.Errorf("streaming unsupported"))
		return
	}

	ctx := r.Context()
	updates, unsubscribe := s.hub.Subscribe()
	defer unsubscribe()

	// policyHub is nil when adminui.policy_addr isn't configured - no
	// policy events to relay, but device updates still work.
	var policyEvents <-chan *api2.PolicyEvent
	if s.policyHub != nil {
		var unsubPolicy func()
		policyEvents, unsubPolicy = s.policyHub.Subscribe()
		defer unsubPolicy()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return

		case update := <-updates:
			if !s.writeDeviceUpdate(w, update) {
				return
			}
			flusher.Flush()

		case ev := <-policyEvents:
			if !s.writePolicyEvent(w, ev) {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) writeDeviceUpdate(w http.ResponseWriter, update *api2.Update) bool {
	du := update.GetDeviceUpdate()
	if du == nil {
		return true
	}

	var frag bytes.Buffer
	var renderErr error
	if update.GetAction() == api2.Update_REMOVED {
		renderErr = fragments["device_info"].ExecuteTemplate(&frag, "device_info_removed", du.GetDeviceId())
	} else {
		renderErr = fragments["device_info"].ExecuteTemplate(&frag, "device_info_oob", deviceToView(du.GetDevice()))
	}
	if renderErr != nil {
		s.logger.Error("template render failed", zap.String("fragment", "device_info"), zap.Error(renderErr))
		return true
	}

	return writeSSEEvent(w, frag.String())
}

// writePolicyEvent renders and writes ev's OOB patch(es). A log_appended
// event writes two log_row_oob rows (the global feed and that policy's own
// feed) in one message, mirroring http.go's pushLogAppended.
func (s *Server) writePolicyEvent(w http.ResponseWriter, ev *api2.PolicyEvent) bool {
	var frag bytes.Buffer
	var renderErr error

	// Switched on which oneof case is actually set, not a value-emptiness
	// check like ev.GetPolicyRemoved() != "" - a removed policy's id is a
	// caller-supplied string with no length restriction, so an empty one
	// would otherwise be indistinguishable from the oneof being unset
	// entirely (falling through to default and silently dropping the
	// event).
	switch ev.GetEvent().(type) {
	case *api2.PolicyEvent_PolicyChanged:
		renderErr = fragments["policy_status"].ExecuteTemplate(&frag, "policy_status_cell_oob", policyToView(ev.GetPolicyChanged()))
	case *api2.PolicyEvent_PolicyRemoved:
		renderErr = fragments["policy_status"].ExecuteTemplate(&frag, "policy_status_removed", ev.GetPolicyRemoved())
	case *api2.PolicyEvent_LogAppended:
		l := executionLogToView(ev.GetLogAppended())
		for _, target := range []string{"#logs-tbody", "#logs-tbody-" + l.PolicyID} {
			data := struct {
				Target string
				Log    executionLogView
			}{Target: target, Log: l}
			if err := fragments["log_row"].ExecuteTemplate(&frag, "log_row_oob", data); err != nil {
				renderErr = err
				break
			}
		}
	default:
		return true
	}

	if renderErr != nil {
		s.logger.Error("template render failed", zap.String("fragment", "policy event"), zap.Error(renderErr))
		return true
	}

	return writeSSEEvent(w, frag.String())
}

// writeSSEEvent writes html as a single SSE "message" event and reports
// whether the write succeeded.
func writeSSEEvent(w http.ResponseWriter, html string) bool {
	_, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", htmxutil.OneLine(html))
	return err == nil
}
