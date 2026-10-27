package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/googlehome/relay"
)

// cameraSignalingRequest matches the body Google's client POSTs directly
// to cameraStreamSignalingUrl (a different endpoint from the fulfillment
// webhook - see developers.home.google.com/cloud-to-cloud/traits/camerastream),
// once EXECUTE/GetCameraStream has returned that URL - see
// relay.RemoteProvider.getCameraStream for where it and its session/token
// pair are minted.
type cameraSignalingRequest struct {
	Action   string `json:"action"`
	DeviceID string `json:"deviceId"`
	SDP      string `json:"sdp"`
}

type cameraSignalingResponse struct {
	Action string `json:"action,omitempty"`
	SDP    string `json:"sdp,omitempty"`
}

// newCameraSignalingHandler serves relay.CameraSignalingPath.
func newCameraSignalingHandler(logger *zap.Logger, sessionMgr *relay.SessionManager, webrtcMgr *relay.WebRTCSessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Required by Google's docs: the caller is a browser-side fetch
		// (the Home app/display surface), not a backend IP - this header
		// matters on every response, not just an OPTIONS preflight.
		w.Header().Set("Access-Control-Allow-Origin", "https://www.gstatic.com")
		if r.Method == http.MethodOptions {
			// The actual request carries an Authorization header and a
			// JSON body, so it's a non-simple cross-origin request - the
			// browser won't send it at all unless the preflight also
			// allows that header and method (Allow-Origin alone isn't
			// enough).
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "POST")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		sessionID := r.URL.Query().Get("session")
		deviceID, ok := sessionMgr.Validate(sessionID, bearerToken(r))
		if !ok {
			http.Error(w, "invalid or expired session", http.StatusUnauthorized)
			return
		}

		var req cameraSignalingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.DeviceID != "" && req.DeviceID != deviceID {
			http.Error(w, "device mismatch", http.StatusUnauthorized)
			return
		}

		switch req.Action {
		case "offer":
			answer, err := webrtcMgr.HandleOffer(sessionID, req.SDP)
			if err != nil {
				logger.Info("unable to handle camera signaling offer", zap.String("session_id", sessionID), zap.Error(err))
				http.Error(w, "unable to negotiate stream", http.StatusInternalServerError)
				return
			}
			// The offer/answer exchange just happened, so the "must
			// arrive within mediaSessionOfferTTL" window is over - extend
			// the token's life past that so a later "end" POST (or ICE
			// still connecting) doesn't get rejected as expired before
			// any RTP has had a chance to arrive and start Touch-ing it
			// itself (see WritePacket).
			sessionMgr.Touch(sessionID)
			writeCameraSignalingJSON(w, cameraSignalingResponse{Action: "answer", SDP: answer})
		case "end":
			// HandleEnd's teardown expires the session record and tells
			// the agent to stop pulling WHEP (see the onTeardown callback
			// wired in main.go) - nothing further to do here.
			webrtcMgr.HandleEnd(sessionID)
			writeCameraSignalingJSON(w, cameraSignalingResponse{})
		default:
			http.Error(w, "unsupported action", http.StatusBadRequest)
		}
	}
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return strings.TrimPrefix(auth, prefix)
}

func writeCameraSignalingJSON(w http.ResponseWriter, v cameraSignalingResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
