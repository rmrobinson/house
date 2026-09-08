package castctrl

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/protobuf/proto"
)

// scriptedDevice extends fakeDevice with canned GET_STATUS responses and the
// ability to push unsolicited status, keyed by which app (transportId) a
// media response/push is associated with.
type scriptedDevice struct {
	fd *fakeDevice

	mu               sync.Mutex
	receiverStatus   receiverStatus
	mediaByTransport map[string][]MediaStatus
	// rejectReceiver/rejectMedia, if non-nil, replace the next reply on their
	// namespace with an INVALID_REQUEST/LOAD_FAILED error response instead of
	// the usual status — consumed (reset to nil) after one use.
	rejectReceiver *errorPayload
	rejectMedia    *errorPayload
}

func newScriptedDevice(t *testing.T) *scriptedDevice {
	fd := newFakeDevice(t)
	sd := &scriptedDevice{fd: fd, mediaByTransport: map[string][]MediaStatus{}}
	fd.onMessage = sd.handle
	return sd
}

func (sd *scriptedDevice) handle(msg *CastMessage, reply func(*CastMessage)) {
	var e envelope
	if err := json.Unmarshal([]byte(msg.GetPayloadUtf8()), &e); err != nil {
		return
	}

	switch msg.GetNamespace() {
	case NamespaceReceiver:
		sd.mu.Lock()
		reject := sd.rejectReceiver
		sd.rejectReceiver = nil
		status := sd.receiverStatus
		sd.mu.Unlock()

		if reject != nil {
			reply(sd.errorMessage(NamespaceReceiver, e.RequestID, *reject))
			return
		}
		if e.Type == TypeGetStatus || e.Type == TypeSetVolume || e.Type == TypeLaunch {
			reply(sd.receiverStatusMessage(e.RequestID, status))
		}
	case NamespaceMedia:
		sd.mu.Lock()
		reject := sd.rejectMedia
		sd.rejectMedia = nil
		statuses := sd.mediaByTransport[msg.GetDestinationId()]
		sd.mu.Unlock()

		if reject != nil {
			reply(sd.errorMessage(NamespaceMedia, e.RequestID, *reject))
			return
		}
		switch e.Type {
		case TypeGetStatus, TypePlay, TypePause, TypeStop, TypeSeek:
			reply(sd.mediaStatusMessage(msg.GetDestinationId(), e.RequestID, statuses))
		}
	}
}

// rejectNextReceiverRequest arranges for the next receiver-namespace request
// (e.g. SET_VOLUME) to get errType/reason back instead of RECEIVER_STATUS.
func (sd *scriptedDevice) rejectNextReceiverRequest(errType, reason string) {
	sd.mu.Lock()
	sd.rejectReceiver = &errorPayload{Type: errType, Reason: reason}
	sd.mu.Unlock()
}

// rejectNextMediaRequest arranges for the next media-namespace request (e.g.
// PLAY/SEEK) to get errType/reason back instead of MEDIA_STATUS.
func (sd *scriptedDevice) rejectNextMediaRequest(errType, reason string) {
	sd.mu.Lock()
	sd.rejectMedia = &errorPayload{Type: errType, Reason: reason}
	sd.mu.Unlock()
}

func (sd *scriptedDevice) errorMessage(namespace string, requestID uint32, e errorPayload) *CastMessage {
	payload := struct {
		Type      string `json:"type"`
		RequestID uint32 `json:"requestId"`
		Reason    string `json:"reason,omitempty"`
	}{Type: e.Type, RequestID: requestID, Reason: e.Reason}
	data, _ := json.Marshal(&payload)

	return &CastMessage{
		ProtocolVersion: CastMessage_CASTV2_1_0.Enum(),
		SourceId:        proto.String(ReceiverID),
		DestinationId:   proto.String(SenderID),
		Namespace:       proto.String(namespace),
		PayloadType:     CastMessage_STRING.Enum(),
		PayloadUtf8:     proto.String(string(data)),
	}
}

func (sd *scriptedDevice) receiverStatusMessage(requestID uint32, status receiverStatus) *CastMessage {
	data, _ := json.Marshal(&receiverStatusPayload{Type: TypeReceiverStatus, RequestID: requestID, Status: status})
	return &CastMessage{
		ProtocolVersion: CastMessage_CASTV2_1_0.Enum(),
		SourceId:        proto.String(ReceiverID),
		DestinationId:   proto.String(SenderID),
		Namespace:       proto.String(NamespaceReceiver),
		PayloadType:     CastMessage_STRING.Enum(),
		PayloadUtf8:     proto.String(string(data)),
	}
}

func (sd *scriptedDevice) mediaStatusMessage(transportID string, requestID uint32, statuses []MediaStatus) *CastMessage {
	data, _ := json.Marshal(&mediaStatusPayload{Type: TypeMediaStatus, RequestID: requestID, Status: statuses})
	return &CastMessage{
		ProtocolVersion: CastMessage_CASTV2_1_0.Enum(),
		SourceId:        proto.String(transportID),
		DestinationId:   proto.String(SenderID),
		Namespace:       proto.String(NamespaceMedia),
		PayloadType:     CastMessage_STRING.Enum(),
		PayloadUtf8:     proto.String(string(data)),
	}
}

func (sd *scriptedDevice) setReceiverStatus(status receiverStatus) {
	sd.mu.Lock()
	sd.receiverStatus = status
	sd.mu.Unlock()
}

func (sd *scriptedDevice) setMediaStatus(transportID string, statuses []MediaStatus) {
	sd.mu.Lock()
	sd.mediaByTransport[transportID] = statuses
	sd.mu.Unlock()
}

func (sd *scriptedDevice) pushReceiverStatus(status receiverStatus) {
	_ = sd.fd.send(sd.receiverStatusMessage(0, status))
}

func (sd *scriptedDevice) pushMediaStatus(transportID string, statuses []MediaStatus) {
	_ = sd.fd.send(sd.mediaStatusMessage(transportID, 0, statuses))
}

// statusRecorder captures the latest Status from a Session's onChange
// callback, safe for concurrent reads from test assertions.
type statusRecorder struct {
	mu   sync.Mutex
	last Status
}

func (r *statusRecorder) record(s Status) {
	r.mu.Lock()
	r.last = s
	r.mu.Unlock()
}

func (r *statusRecorder) get() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func runSession(t *testing.T, sd *scriptedDevice) (*Session, *statusRecorder) {
	rec := &statusRecorder{}
	sess := newSession(zaptest.NewLogger(t), sd.fd.addr(), dialTo(sd.fd), rec.record)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sess.Run(ctx)

	return sess, rec
}

func TestSession_InitialConnectReachesAppConnected(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: "CC1AD845", TransportID: "app-1"}},
		Volume:       ReceiverVolume{Level: 0.5, ControlType: "master", StepInterval: 0.02},
	})
	sd.setMediaStatus("app-1", []MediaStatus{{MediaSessionID: 42, PlayerState: "PLAYING"}})

	_, rec := runSession(t, sd)

	require.Eventually(t, func() bool {
		st := rec.get()
		return st.State == StateAppConnected && st.HasMedia
	}, 2*time.Second, 10*time.Millisecond)

	st := rec.get()
	assert.Equal(t, 42, st.Media.MediaSessionID)
	assert.Equal(t, "app-1", st.App.TransportID)
	assert.Equal(t, "master", st.Volume.ControlType)
}

func TestSession_TransportIDChangeTriggersInnerReconnect(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: "A1", TransportID: "app-1"}},
	})
	sd.setMediaStatus("app-1", []MediaStatus{{MediaSessionID: 1, PlayerState: "PLAYING"}})

	_, rec := runSession(t, sd)

	require.Eventually(t, func() bool {
		return rec.get().Media.MediaSessionID == 1
	}, 2*time.Second, 10*time.Millisecond)

	// A new app launches: different transportId, different media session.
	// Only reachable if Session re-runs the inner CONNECT + GET_STATUS
	// against the new transportId — re-querying the old one would never see
	// mediaSessionId 2, since it's only registered under "app-2".
	sd.setMediaStatus("app-2", []MediaStatus{{MediaSessionID: 2, PlayerState: "PLAYING"}})
	sd.pushReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: "A2", TransportID: "app-2"}},
	})

	require.Eventually(t, func() bool {
		return rec.get().Media.MediaSessionID == 2
	}, 2*time.Second, 10*time.Millisecond)

	st := rec.get()
	assert.Equal(t, "app-2", st.App.TransportID)
	assert.Equal(t, StateAppConnected, st.State)
}

func TestSession_EmptyApplicationsDropsToReceiverConnected(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: "A1", TransportID: "app-1"}},
	})
	sd.setMediaStatus("app-1", []MediaStatus{{MediaSessionID: 7, PlayerState: "PLAYING"}})

	_, rec := runSession(t, sd)

	require.Eventually(t, func() bool {
		return rec.get().State == StateAppConnected
	}, 2*time.Second, 10*time.Millisecond)

	sd.pushReceiverStatus(receiverStatus{Applications: nil})

	require.Eventually(t, func() bool {
		st := rec.get()
		return st.State == StateReceiverConnected && !st.HasMedia
	}, 2*time.Second, 10*time.Millisecond)
}

func TestSession_MediaSessionEndedClearsMediaSessionID(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: "A1", TransportID: "app-1"}},
	})
	sd.setMediaStatus("app-1", []MediaStatus{{MediaSessionID: 9, PlayerState: "PLAYING"}})

	_, rec := runSession(t, sd)

	require.Eventually(t, func() bool {
		st := rec.get()
		return st.HasMedia && st.Media.MediaSessionID == 9
	}, 2*time.Second, 10*time.Millisecond)

	sd.pushMediaStatus("app-1", nil) // empty status array: media session ended

	require.Eventually(t, func() bool {
		return !rec.get().HasMedia
	}, 2*time.Second, 10*time.Millisecond)

	assert.Equal(t, 0, rec.get().Media.MediaSessionID)
}

func TestSession_StaleMediaStatusFromPreviousAppIgnored(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: "A1", TransportID: "app-1"}},
	})
	sd.setMediaStatus("app-1", []MediaStatus{{MediaSessionID: 1, PlayerState: "PLAYING"}})
	sd.setMediaStatus("app-2", []MediaStatus{{MediaSessionID: 2, PlayerState: "PLAYING"}})

	_, rec := runSession(t, sd)

	require.Eventually(t, func() bool {
		return rec.get().Media.MediaSessionID == 1
	}, 2*time.Second, 10*time.Millisecond)

	sd.pushReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: "A2", TransportID: "app-2"}},
	})
	require.Eventually(t, func() bool {
		return rec.get().Media.MediaSessionID == 2
	}, 2*time.Second, 10*time.Millisecond)

	// A push claiming to be from app-1 — an app connection we've since moved
	// on from — must not resurrect its stale mediaSessionId.
	sd.pushMediaStatus("app-1", []MediaStatus{{MediaSessionID: 1, PlayerState: "PLAYING"}})

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 2, rec.get().Media.MediaSessionID, "stale media status from a previous app must not overwrite the current session")
}

func TestSession_WriteMethodsRequireActiveMediaSession(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{}) // no app running

	sess, rec := runSession(t, sd)

	require.Eventually(t, func() bool {
		return rec.get().State == StateReceiverConnected
	}, 2*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	assert.ErrorIs(t, sess.Play(ctx), ErrNoActiveApp)
	assert.ErrorIs(t, sess.Pause(ctx), ErrNoActiveApp)
	assert.ErrorIs(t, sess.SeekAbsolute(ctx, 10), ErrNoActiveApp)
}

// TestSession_MediaStatusWithoutMediaObjectRetainsPreviousMetadata guards
// against a real bug found live: a device only sends the nested `media`
// object on the status push that follows a LOAD. Later pushes for the same
// mediaSessionId (e.g. after PLAY/PAUSE) omit it, and a naive wholesale
// replace of Session.media loses previously known title/artist/art.
func TestSession_MediaStatusWithoutMediaObjectRetainsPreviousMetadata(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: "CC32E753", TransportID: "app-1"}},
	})
	sd.setMediaStatus("app-1", []MediaStatus{{
		MediaSessionID: 1,
		PlayerState:    "PLAYING",
		Media: &MediaInformation{
			ContentID: "spotify:track:abc",
			Duration:  165,
			Metadata:  &MediaMetadata{MetadataType: 3, Title: "Sports car", Artist: "Tate McRae"},
		},
	}})

	_, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		st := rec.get()
		return st.HasMedia && st.Media.Media != nil
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, "Sports car", rec.get().Media.Media.Metadata.Title)

	// A later push for the same media session (e.g. the one PAUSE triggers)
	// omits the nested media object entirely, as real Cast/Spotify sessions
	// do.
	sd.pushMediaStatus("app-1", []MediaStatus{{
		MediaSessionID: 1,
		PlayerState:    "PAUSED",
		CurrentTime:    42,
	}})

	require.Eventually(t, func() bool {
		return rec.get().Media.PlayerState == "PAUSED"
	}, 2*time.Second, 10*time.Millisecond)

	st := rec.get()
	require.NotNil(t, st.Media.Media, "metadata must be retained across a status push that omits the media object")
	assert.Equal(t, "Sports car", st.Media.Media.Metadata.Title)
	assert.Equal(t, "Tate McRae", st.Media.Media.Metadata.Artist)
	assert.Equal(t, 42.0, st.Media.CurrentTime, "fields the push did include must still update normally")
}

// TestSession_MediaStatusNewSessionWithoutMediaObjectDoesNotInheritStaleMetadata
// is the flip side: retention must be scoped to the same mediaSessionId, or a
// genuinely new track that happens to omit `media` would incorrectly show the
// previous track's metadata.
func TestSession_MediaStatusNewSessionWithoutMediaObjectDoesNotInheritStaleMetadata(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: "CC32E753", TransportID: "app-1"}},
	})
	sd.setMediaStatus("app-1", []MediaStatus{{
		MediaSessionID: 1,
		PlayerState:    "PLAYING",
		Media: &MediaInformation{
			Duration: 165,
			Metadata: &MediaMetadata{MetadataType: 3, Title: "Sports car"},
		},
	}})

	_, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		st := rec.get()
		return st.HasMedia && st.Media.Media != nil
	}, 2*time.Second, 10*time.Millisecond)

	// A different mediaSessionId (new track) that also omits media must not
	// inherit the previous track's metadata.
	sd.pushMediaStatus("app-1", []MediaStatus{{
		MediaSessionID: 2,
		PlayerState:    "PLAYING",
	}})

	require.Eventually(t, func() bool {
		return rec.get().Media.MediaSessionID == 2
	}, 2*time.Second, 10*time.Millisecond)

	assert.Nil(t, rec.get().Media.Media, "a different media session must not inherit the previous session's metadata")
}

func TestAsRejection(t *testing.T) {
	msg := func(payload string) *CastMessage {
		return &CastMessage{PayloadUtf8: proto.String(payload)}
	}

	assert.NoError(t, asRejection(msg(`{"type":"RECEIVER_STATUS","requestId":1,"status":{}}`)))
	assert.NoError(t, asRejection(msg(`{"type":"MEDIA_STATUS","requestId":1,"status":[]}`)))
	assert.NoError(t, asRejection(msg(`not json`)))

	err := asRejection(msg(`{"type":"INVALID_REQUEST","requestId":1,"reason":"INVALID_PARAMS"}`))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRequestRejected)
	assert.Contains(t, err.Error(), "INVALID_PARAMS")

	err = asRejection(msg(`{"type":"LOAD_FAILED","requestId":1}`))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRequestRejected)
}

// TestAsRejection_RealDeviceCapture uses the literal, unmodified response a
// real Google Home Mini sent back for a SET_VOLUME-shaped request with an
// unrecognized "type" (captured live against 10.17.15.104 by hand-crafting a
// raw request via a throwaway tool built on castctrl.Conn directly). It has
// no "status" field at all — confirming the failure mode this whole
// asRejection mechanism exists to prevent: unmarshaling this straight into
// receiverStatusPayload (the pre-fix behavior) would silently produce a
// zero-valued Volume/Applications and overwrite whatever was previously
// known, rather than surfacing the rejection.
func TestAsRejection_RealDeviceCapture(t *testing.T) {
	realCapture := `{"reason":"INVALID_COMMAND","requestId":1,"type":"INVALID_REQUEST"}`

	err := asRejection(&CastMessage{PayloadUtf8: proto.String(realCapture)})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRequestRejected)
	assert.Contains(t, err.Error(), "INVALID_COMMAND")

	// And confirm the shape really would have zeroed out receiverStatus.
	var asStatus receiverStatusPayload
	require.NoError(t, json.Unmarshal([]byte(realCapture), &asStatus))
	assert.Empty(t, asStatus.Status.Applications)
	assert.Zero(t, asStatus.Status.Volume)
}

// TestSession_SetVolumeLevelRejected verifies that a device answering
// SET_VOLUME with INVALID_REQUEST surfaces as an error and — the specific bug
// this guards against — doesn't get unmarshaled into receiverStatusPayload
// and overwrite the cached volume with that message's zero-valued Volume
// field.
func TestSession_SetVolumeLevelRejected(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{
		Volume: ReceiverVolume{Level: 0.5, ControlType: "attenuation", StepInterval: 0.1},
	})

	sess, rec := runSession(t, sd)

	require.Eventually(t, func() bool {
		return rec.get().State == StateReceiverConnected
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, 0.5, sess.Status().Volume.Level)

	// "INVALID_COMMAND" is the real reason a Google Home Mini sent back live
	// for an unrecognized receiver-namespace command type - see
	// TestAsRejection_RealDeviceCapture.
	sd.rejectNextReceiverRequest(TypeInvalidRequest, "INVALID_COMMAND")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := sess.SetVolumeLevel(ctx, 0.9)

	require.ErrorIs(t, err, ErrRequestRejected)
	assert.Equal(t, 0.5, sess.Status().Volume.Level,
		"a rejected SET_VOLUME must not overwrite the cached volume with the error response's zero value")
}

// TestSession_LaunchApp verifies LaunchApp sends LAUNCH with the requested
// appId and applies the resulting RECEIVER_STATUS — including connecting to
// the newly launched app, via the same transportId-change machinery an
// unsolicited push after another sender's launch would trigger.
func TestSession_LaunchApp(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{}) // no app running yet
	sd.setMediaStatus("app-1", []MediaStatus{{MediaSessionID: 1, PlayerState: "PLAYING"}})

	sess, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		return rec.get().State == StateReceiverConnected
	}, 2*time.Second, 10*time.Millisecond)

	// LAUNCH's own response reports the newly running app.
	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: "233637DE", TransportID: "app-1"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, sess.LaunchApp(ctx, "233637DE"))

	require.Eventually(t, func() bool {
		st := rec.get()
		return st.State == StateAppConnected && st.App.AppID == "233637DE"
	}, 2*time.Second, 10*time.Millisecond)
}

// TestSession_LaunchAppRejected verifies a LAUNCH_ERROR response surfaces as
// ErrRequestRejected rather than being mistaken for a RECEIVER_STATUS.
func TestSession_LaunchAppRejected(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{})

	sess, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		return rec.get().State == StateReceiverConnected
	}, 2*time.Second, 10*time.Millisecond)

	sd.rejectNextReceiverRequest(TypeLaunchError, "NOT_FOUND")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := sess.LaunchApp(ctx, "NOSUCHAPP")

	require.ErrorIs(t, err, ErrRequestRejected)
	assert.Empty(t, sess.Status().App.AppID)
}

// TestSession_SeekRejected is the media-namespace equivalent of
// TestSession_SetVolumeLevelRejected: a LOAD_FAILED/INVALID_REQUEST reply to
// a media command must not be mistaken for an (empty) MEDIA_STATUS and clear
// the cached media session.
func TestSession_SeekRejected(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: "A1", TransportID: "app-1"}},
	})
	sd.setMediaStatus("app-1", []MediaStatus{{MediaSessionID: 1, PlayerState: "PLAYING", CurrentTime: 30}})

	sess, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		st := rec.get()
		return st.HasMedia && st.Media.MediaSessionID == 1
	}, 2*time.Second, 10*time.Millisecond)

	sd.rejectNextMediaRequest(TypeLoadFailed, "")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := sess.SeekAbsolute(ctx, 60)

	require.ErrorIs(t, err, ErrRequestRejected)
	assert.Equal(t, 1, sess.Status().Media.MediaSessionID,
		"a rejected SEEK must not clear the cached media session")
}
