// media_session.go handles the one thing translate.go's usual
// deviceToAction/stateToAction/buildCommand dispatch can't: a Camera's
// GetCameraStream session. relay.RemoteProvider.Execute (the googlehomed
// side) intercepts that command before it ever reaches this agent's
// generic BridgeService command path and instead sends a
// StartMediaSession down the control stream - this file is what responds
// to that: pull the device's local WHEP endpoint (already published in
// Device.Camera.MediaStream.State.Endpoints - see bridges/frigate/
// camera.go for the one bridge that does today) and forward the RTP it
// receives up to googlehomed over a dedicated PushMediaSession stream,
// until StopMediaSession, the WHEP track ending, or this agent's own
// relay connection dropping.
//
// Video-only, H264 only, matching relay/webrtc_session.go's pinned codec
// on the googlehomed side - see its doc comment for why (the two legs are
// negotiated independently, and this agent has no way to transcode if
// they disagree, so it fails closed via MediaSessionFailed instead).
package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/googlehome/relay"
)

// whepTrackTimeout bounds how long startMediaSession waits for go2rtc to
// actually start sending RTP after a successful WHEP offer/answer, before
// giving up and reporting MediaSessionFailed.
const whepTrackTimeout = 10 * time.Second

// startMediaSession handles one StartMediaSession control message: pulls
// msg's device's WHEP endpoint, and on success starts forwarding its RTP
// to googlehomed and replies MediaSessionReady; on any failure replies
// MediaSessionFailed. Runs on its own goroutine (see agent.go's
// processRelayRequests) since the WHEP pull is a real network round trip
// that has nothing to do with the relay stream's other traffic.
func (a *Agent) startMediaSession(ctx context.Context, stream relay.RelayService_StateSyncClient, requestID string, msg *relay.StartMediaSession) {
	sessionID := msg.GetSessionId()
	sessionCtx, cancel := context.WithCancel(ctx)

	a.sessionsMu.Lock()
	a.mediaSessions[sessionID] = cancel
	a.sessionsMu.Unlock()

	fail := func(errMsg string) {
		cancel()
		a.sessionsMu.Lock()
		delete(a.mediaSessions, sessionID)
		a.sessionsMu.Unlock()

		a.logger.Info("media session failed", zap.String("session_id", sessionID), zap.String("error", errMsg))
		if err := a.send(stream, &relay.ClientRequest{
			RequestId: requestID,
			Field:     &relay.ClientRequest_MediaSessionFailed{MediaSessionFailed: &relay.MediaSessionFailed{SessionId: sessionID, Error: errMsg}},
		}); err != nil {
			a.logger.Error("unable to send media session failure", zap.String("session_id", sessionID), zap.Error(err))
		}
	}

	client := a.bridgeConn.Client()
	if client == nil {
		fail("bridge connection not ready")
		return
	}
	d, err := client.GetDevice(sessionCtx, &api2.GetDeviceRequest{Id: msg.GetDeviceId()})
	if err != nil {
		fail("device not found")
		return
	}
	whepURL := findWHEPEndpoint(d)
	if whepURL == "" {
		fail("device has no WHEP endpoint")
		return
	}

	pc, resourceURL, track, err := pullWHEP(sessionCtx, whepURL)
	if err != nil {
		fail(fmt.Sprintf("WHEP pull failed: %v", err))
		return
	}
	if track.Codec().MimeType != webrtc.MimeTypeH264 {
		_ = pc.Close()
		teardownWHEP(resourceURL)
		fail(fmt.Sprintf("unsupported codec %q from WHEP endpoint, only H264 is forwarded", track.Codec().MimeType))
		return
	}

	pushStream, err := a.relayClient.PushMediaSession(sessionCtx)
	if err != nil {
		_ = pc.Close()
		teardownWHEP(resourceURL)
		fail(fmt.Sprintf("unable to open media push stream: %v", err))
		return
	}

	if err := a.send(stream, &relay.ClientRequest{
		RequestId: requestID,
		Field:     &relay.ClientRequest_MediaSessionReady{MediaSessionReady: &relay.MediaSessionReady{SessionId: sessionID}},
	}); err != nil {
		a.logger.Error("unable to send media session ready", zap.String("session_id", sessionID), zap.Error(err))
	}

	a.forwardMediaSession(sessionCtx, sessionID, pc, resourceURL, track, pushStream)
}

// stopMediaSession cancels sessionID's forwarding loop, if it's still
// running - a no-op if it already stopped on its own (e.g. the WHEP track
// ended, or it never got past startMediaSession's own failure handling).
func (a *Agent) stopMediaSession(sessionID string) {
	a.sessionsMu.Lock()
	cancel, ok := a.mediaSessions[sessionID]
	a.sessionsMu.Unlock()
	if ok {
		cancel()
	}
}

// forwardMediaSession reads RTP from track and pushes every packet up to
// googlehomed over pushStream until sessionCtx is cancelled (via
// stopMediaSession, or this agent's relay connection going down) or the
// WHEP track itself ends, whichever comes first. Always closes pc, tears
// down the WHEP session, and removes the session's cancel func.
func (a *Agent) forwardMediaSession(sessionCtx context.Context, sessionID string, pc *webrtc.PeerConnection, resourceURL string, track *webrtc.TrackRemote, pushStream relay.RelayService_PushMediaSessionClient) {
	defer func() {
		a.sessionsMu.Lock()
		delete(a.mediaSessions, sessionID)
		a.sessionsMu.Unlock()

		_ = pc.Close()
		teardownWHEP(resourceURL)
		_, _ = pushStream.CloseAndRecv()
	}()

	// track.ReadRTP below only returns once pc is closed (or a packet
	// arrives) - closing it is what makes sessionCtx cancellation actually
	// interrupt a blocked read.
	go func() {
		<-sessionCtx.Done()
		_ = pc.Close()
	}()

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if sessionCtx.Err() == nil {
				a.logger.Info("WHEP track ended", zap.String("session_id", sessionID), zap.Error(err))
			}
			return
		}
		raw, err := pkt.Marshal()
		if err != nil {
			continue
		}
		if err := pushStream.Send(&relay.MediaPacket{SessionId: sessionID, Rtp: raw}); err != nil {
			a.logger.Info("unable to forward media packet", zap.String("session_id", sessionID), zap.Error(err))
			return
		}
	}
}

// findWHEPEndpoint returns d's WEBRTC_WHEP MediaStream endpoint URL, or ""
// if it has none (not a Camera, or a Camera whose bridge doesn't publish
// one - see bridges/frigate/camera.go for the one bridge that does today).
func findWHEPEndpoint(d *device.Device) string {
	for _, ep := range d.GetCamera().GetMediaStream().GetState().GetEndpoints() {
		if ep.GetProtocol() == trait.MediaStream_WEBRTC_WHEP {
			return ep.GetUrl()
		}
	}
	return ""
}

// pullWHEP opens a pion PeerConnection as a WHEP client against whepURL
// (recvonly video only), waits for its one media track to arrive, and
// returns it along with the per-session resource URL (from the WHEP
// response's Location header) that teardownWHEP later DELETEs. The
// caller owns closing pc.
func pullWHEP(ctx context.Context, whepURL string) (pc *webrtc.PeerConnection, resourceURL string, track *webrtc.TrackRemote, err error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, "", nil, err
	}

	pc, err = webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, "", nil, err
	}

	trackCh := make(chan *webrtc.TrackRemote, 1)
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		select {
		case trackCh <- t:
		default:
		}
	})

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		_ = pc.Close()
		return nil, "", nil, err
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, "", nil, err
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		_ = pc.Close()
		return nil, "", nil, err
	}
	<-gatherComplete

	answerSDP, resourceURL, err := postWHEPOffer(ctx, whepURL, pc.LocalDescription().SDP)
	if err != nil {
		_ = pc.Close()
		return nil, "", nil, err
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}); err != nil {
		_ = pc.Close()
		return nil, "", nil, err
	}

	select {
	case track = <-trackCh:
	case <-time.After(whepTrackTimeout):
		_ = pc.Close()
		return nil, "", nil, fmt.Errorf("agent: no media track received from %s within %s", whepURL, whepTrackTimeout)
	case <-ctx.Done():
		_ = pc.Close()
		return nil, "", nil, ctx.Err()
	}

	return pc, resourceURL, track, nil
}

// postWHEPOffer POSTs offerSDP to whepURL per the WHEP spec, returning the
// SDP answer and the per-session resource URL (from the Location header,
// resolved against whepURL if it came back relative).
func postWHEPOffer(ctx context.Context, whepURL, offerSDP string) (answerSDP, resourceURL string, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, whepURL, strings.NewReader(offerSDP))
	if err != nil {
		return "", "", err
	}
	httpReq.Header.Set("Content-Type", "application/sdp")

	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", "", err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("WHEP endpoint returned %s", httpResp.Status)
	}

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return "", "", err
	}

	resourceURL = httpResp.Header.Get("Location")
	if resourceURL != "" {
		if base, err := url.Parse(whepURL); err == nil {
			if loc, err := url.Parse(resourceURL); err == nil {
				resourceURL = base.ResolveReference(loc).String()
			}
		}
	}

	return string(body), resourceURL, nil
}

// teardownWHEPTimeout bounds the best-effort DELETE below - http.DefaultClient
// has no default timeout, so an unresponsive go2rtc would otherwise block
// forwardMediaSession's deferred cleanup (and the PushMediaSession stream
// it's closing) indefinitely.
const teardownWHEPTimeout = 5 * time.Second

// teardownWHEP DELETEs resourceURL, best-effort, so go2rtc frees the WHEP
// session promptly rather than waiting out its own idle timeout.
func teardownWHEP(resourceURL string) {
	if resourceURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), teardownWHEPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, resourceURL, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
