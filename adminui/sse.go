package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
)

// handleSSE relays BridgeService.StreamUpdates as Server-Sent Events for
// htmx's SSE extension (see templates/layout.html's hx-ext="sse"). Every
// page shows a device's name/kind/online status in a
// <td id="device-info-<id>"> cell (see templates/partials/device_info.html),
// separate from any room-link action cell around it, which is
// page-specific (room page: unlink; devices page: link/move) and untouched
// by a device-state update. A DeviceUpdate is re-emitted as that same cell
// carrying hx-swap-oob, so htmx swaps it wherever it currently exists in the
// DOM - no page-specific subscription bookkeeping needed.
// BridgeUpdate/CommandUpdate/InitialUpdate are not relayed: this admin UI
// has no view of bridge-level or in-flight command state, only device info.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.httpError(w, r, fmt.Errorf("streaming unsupported"))
		return
	}

	ctx := r.Context()
	stream, err := s.bridge.StreamUpdates(ctx, &api2.StreamUpdatesRequest{})
	if err != nil {
		s.httpError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		update, err := stream.Recv()
		if err != nil {
			// ctx is r.Context(), so ctx.Err() != nil means the client
			// itself went away (tab closed, or - since top-level navigation
			// is a plain <a href>, not htmx-boosted, see server.go - simply
			// navigated to the next page) and StreamUpdates was cancelled
			// as a result. That's the expected end of every SSE connection,
			// not a failure worth a warning, unlike the stream actually
			// ending on the bridge-facade side.
			if err != io.EOF && ctx.Err() == nil {
				s.logger.Warn("bridge update stream ended", zap.Error(err))
			}
			return
		}

		du := update.GetDeviceUpdate()
		if du == nil {
			continue
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
			continue
		}

		if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", oneLine(frag.String())); err != nil {
			return
		}
		flusher.Flush()

		if ctx.Err() != nil {
			return
		}
	}
}

// oneLine collapses s to a single line - the SSE wire format terminates a
// data field at the first newline, so a multi-line HTML fragment must be
// sent as consecutive "data: " lines instead of one. html/template's output
// here is compact enough (see templates/partials/device_row.html) that
// stripping newlines outright is simpler than splitting into multiple
// "data:" lines and doesn't change the rendered HTML.
func oneLine(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}
