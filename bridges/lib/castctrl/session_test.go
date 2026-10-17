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
	// launchAsStatus makes LAUNCH requests get a LAUNCH_STATUS reply (string
	// status, same requestId) instead of a RECEIVER_STATUS - what a real
	// Home Mini sent in a live test. The app then only shows up via a later
	// push.
	launchAsStatus bool
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
		launchAsStatus := sd.launchAsStatus
		sd.mu.Unlock()

		if reject != nil {
			reply(sd.errorMessage(NamespaceReceiver, e.RequestID, *reject))
			return
		}
		if e.Type == TypeLaunch && launchAsStatus {
			reply(sd.launchStatusMessage(e.RequestID))
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
		case TypeGetStatus, TypePlay, TypePause, TypeStop, TypeSeek, TypeLoad, TypeQueueLoad:
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

// launchStatusMessage is a LAUNCH_STATUS: its "status" is a string, so it
// cannot be decoded as a RECEIVER_STATUS.
func (sd *scriptedDevice) launchStatusMessage(requestID uint32) *CastMessage {
	data, _ := json.Marshal(map[string]any{"type": TypeLaunchStatus, "requestId": requestID, "status": "USER_REQUESTED"})
	return &CastMessage{
		ProtocolVersion: CastMessage_CASTV2_1_0.Enum(),
		SourceId:        proto.String(ReceiverID),
		DestinationId:   proto.String(SenderID),
		Namespace:       proto.String(NamespaceReceiver),
		PayloadType:     CastMessage_STRING.Enum(),
		PayloadUtf8:     proto.String(string(data)),
	}
}

func (sd *scriptedDevice) setLaunchAsStatus(v bool) {
	sd.mu.Lock()
	sd.launchAsStatus = v
	sd.mu.Unlock()
}

// pushLaunchError sends an unsolicited LAUNCH_ERROR (no pending requestId).
func (sd *scriptedDevice) pushLaunchError(reason string) {
	_ = sd.fd.send(sd.errorMessage(NamespaceReceiver, 0, errorPayload{Type: TypeLaunchError, Reason: reason}))
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
	mu     sync.Mutex
	last   Status
	volume []float64 // Volume.Level of every Status recorded, in order
}

func (r *statusRecorder) record(s Status) {
	r.mu.Lock()
	r.last = s
	r.volume = append(r.volume, s.Volume.Level)
	r.mu.Unlock()
}

// levels returns the volume level of every Status recorded after the first
// one that had a non-zero level (earlier ones predate the first receiver
// status, so a zero there is just "not yet known").
func (r *statusRecorder) levels() []float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, v := range r.volume {
		if v != 0 {
			return append([]float64(nil), r.volume[i:]...)
		}
	}
	return nil
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

// sentMediaPayloads returns the decoded JSON of every media-namespace request
// of the given type the fake device received.
func (sd *scriptedDevice) sentMediaPayloads(t *testing.T, typ string) []map[string]any {
	sd.fd.mu.Lock()
	defer sd.fd.mu.Unlock()
	var out []map[string]any
	for _, msg := range sd.fd.received {
		if msg.GetNamespace() != NamespaceMedia {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(msg.GetPayloadUtf8()), &m))
		if m["type"] == typ {
			out = append(out, m)
		}
	}
	return out
}

func (sd *scriptedDevice) sentReceiverTypes() []string {
	sd.fd.mu.Lock()
	defer sd.fd.mu.Unlock()
	var out []string
	for _, msg := range sd.fd.received {
		if msg.GetNamespace() != NamespaceReceiver {
			continue
		}
		var e envelope
		_ = json.Unmarshal([]byte(msg.GetPayloadUtf8()), &e)
		out = append(out, e.Type)
	}
	return out
}

// TestSession_LoadMedia_IdleDeviceLaunchesDefaultMediaReceiver verifies the
// 10pm case: nothing is running, so LoadMedia must launch the media receiver
// and then LOAD to its transport.
func TestSession_LoadMedia_IdleDeviceLaunchesDefaultMediaReceiver(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{}) // idle
	sd.setMediaStatus("dmr-1", []MediaStatus{{MediaSessionID: 1, PlayerState: "PLAYING"}})

	sess, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		return rec.get().State == StateReceiverConnected
	}, 2*time.Second, 10*time.Millisecond)

	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: DefaultMediaReceiverAppID, TransportID: "dmr-1"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, sess.LoadMedia(ctx, LoadRequest{
		ContentURL:  "https://example.com/a.mp3",
		ContentType: "audio/mp3",
		Title:       "Rain",
	}))

	assert.Contains(t, sd.sentReceiverTypes(), TypeLaunch)
	loads := sd.sentMediaPayloads(t, TypeLoad)
	require.Len(t, loads, 1)
	media := loads[0]["media"].(map[string]any)
	assert.Equal(t, "https://example.com/a.mp3", media["contentId"])
	assert.Equal(t, "audio/mp3", media["contentType"])
	assert.Equal(t, true, loads[0]["autoplay"])

	st := sess.Status()
	assert.True(t, st.HasMedia)
	assert.Equal(t, "PLAYING", st.Media.PlayerState)
}

// TestSession_LoadMedia_AlreadyOnDefaultMediaReceiverSkipsLaunch verifies the
// receiver isn't relaunched (which would cut off its connection) when it is
// already the foreground app.
func TestSession_LoadMedia_AlreadyOnDefaultMediaReceiverSkipsLaunch(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: DefaultMediaReceiverAppID, TransportID: "dmr-1"}},
	})
	sd.setMediaStatus("dmr-1", []MediaStatus{{MediaSessionID: 1, PlayerState: "PLAYING"}})

	sess, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		return rec.get().State == StateAppConnected
	}, 2*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, sess.LoadMedia(ctx, LoadRequest{ContentURL: "https://example.com/a.mp3", ContentType: "audio/mp3"}))

	assert.NotContains(t, sd.sentReceiverTypes(), TypeLaunch)
	assert.Len(t, sd.sentMediaPayloads(t, TypeLoad), 1)
}

// TestSession_LoadMedia_RepeatUsesQueueLoad verifies looping is requested via
// a REPEAT_SINGLE queue, since LOAD has no repeat flag.
func TestSession_LoadMedia_RepeatUsesQueueLoad(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: DefaultMediaReceiverAppID, TransportID: "dmr-1"}},
	})
	sd.setMediaStatus("dmr-1", []MediaStatus{{MediaSessionID: 1, PlayerState: "PLAYING"}})

	sess, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		return rec.get().State == StateAppConnected
	}, 2*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, sess.LoadMedia(ctx, LoadRequest{ContentURL: "https://example.com/a.mp3", ContentType: "audio/mp3", Repeat: true}))

	assert.Empty(t, sd.sentMediaPayloads(t, TypeLoad))
	q := sd.sentMediaPayloads(t, TypeQueueLoad)
	require.Len(t, q, 1)
	assert.Equal(t, "REPEAT_SINGLE", q[0]["repeatMode"])
	items := q[0]["items"].([]any)
	require.Len(t, items, 1)
	assert.Equal(t, "https://example.com/a.mp3", items[0].(map[string]any)["media"].(map[string]any)["contentId"])
}

func TestSession_LoadMedia_LoadFailed(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: DefaultMediaReceiverAppID, TransportID: "dmr-1"}},
	})
	sd.setMediaStatus("dmr-1", []MediaStatus{{MediaSessionID: 1, PlayerState: "PLAYING"}})

	sess, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		return rec.get().State == StateAppConnected
	}, 2*time.Second, 10*time.Millisecond)

	sd.rejectNextMediaRequest(TypeLoadFailed, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := sess.LoadMedia(ctx, LoadRequest{ContentURL: "https://example.com/a.mp3", ContentType: "audio/mp3"})
	require.ErrorIs(t, err, ErrRequestRejected)
}

// TestSession_LoadMedia_WaitsForAppAfterEarlyLaunchReply covers a LAUNCH reply
// that arrives before the app has a transportId: the connection only happens
// when the receiver's follow-up push does, so LoadMedia must wait for it
// rather than fail.
func TestSession_LoadMedia_WaitsForAppAfterEarlyLaunchReply(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{}) // the LAUNCH reply still shows no app
	sd.setMediaStatus("dmr-1", []MediaStatus{{MediaSessionID: 1, PlayerState: "PLAYING"}})

	sess, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		return rec.get().State == StateReceiverConnected
	}, 2*time.Second, 10*time.Millisecond)

	withApp := receiverStatus{
		Applications: []ReceiverApplication{{AppID: DefaultMediaReceiverAppID, TransportID: "dmr-1"}},
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		sd.setReceiverStatus(withApp)
		sd.pushReceiverStatus(withApp)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, sess.LoadMedia(ctx, LoadRequest{ContentURL: "https://example.com/a.mp3", ContentType: "audio/mp3"}))
	assert.Len(t, sd.sentMediaPayloads(t, TypeLoad), 1)
}

// TestSession_LoadMedia_AppNeverArrives verifies LoadMedia gives up with
// ErrNoActiveApp at the caller's deadline, and never sends a LOAD into the void.
func TestSession_LoadMedia_AppNeverArrives(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{})

	sess, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		return rec.get().State == StateReceiverConnected
	}, 2*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := sess.LoadMedia(ctx, LoadRequest{ContentURL: "https://example.com/a.mp3", ContentType: "audio/mp3"})
	require.ErrorIs(t, err, ErrNoActiveApp)
	// A timeout must still read as one, not just as "no app".
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, sd.sentMediaPayloads(t, TypeLoad))
}

// loadDestinations returns the transport each LOAD/QUEUE_LOAD was addressed to.
func (sd *scriptedDevice) loadDestinations() []string {
	sd.fd.mu.Lock()
	defer sd.fd.mu.Unlock()
	var out []string
	for _, msg := range sd.fd.received {
		if msg.GetNamespace() != NamespaceMedia {
			continue
		}
		var e envelope
		_ = json.Unmarshal([]byte(msg.GetPayloadUtf8()), &e)
		if e.Type == TypeLoad || e.Type == TypeQueueLoad {
			out = append(out, msg.GetDestinationId())
		}
	}
	return out
}

// TestSession_LoadMedia_SwitchFromOtherApp is the 10pm case when something
// else (Spotify, say) is playing: the LAUNCH reply is a LAUNCH_STATUS, and the
// Default Media Receiver only appears later via a push. The LOAD must go to
// the new app's transport, never the old one's.
func TestSession_LoadMedia_SwitchFromOtherApp(t *testing.T) {
	sd := newScriptedDevice(t)
	old := receiverStatus{
		Applications: []ReceiverApplication{{AppID: "CAFE0001", TransportID: "app-old"}},
	}
	sd.setReceiverStatus(old)
	sd.setLaunchAsStatus(true)
	sd.setMediaStatus("app-old", []MediaStatus{{MediaSessionID: 7, PlayerState: "PLAYING"}})
	sd.setMediaStatus("dmr-1", []MediaStatus{{MediaSessionID: 1, PlayerState: "PLAYING"}})

	sess, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		st := rec.get()
		return st.State == StateAppConnected && st.App.AppID == "CAFE0001"
	}, 2*time.Second, 10*time.Millisecond)

	withDMR := receiverStatus{
		Applications: []ReceiverApplication{{AppID: DefaultMediaReceiverAppID, TransportID: "dmr-1"}},
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		sd.setReceiverStatus(withDMR)
		sd.pushReceiverStatus(withDMR)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, sess.LoadMedia(ctx, LoadRequest{ContentURL: "https://example.com/a.mp3", ContentType: "audio/mp3"}))
	assert.Equal(t, []string{"dmr-1"}, sd.loadDestinations())
}

// TestSession_ConnectedToAppRequiresMatchingTransport pins the window where
// applyReceiverStatus has recorded the new app but connectApp has not yet moved
// appTransportID over: the session must not count as connected to the new app.
func TestSession_ConnectedToAppRequiresMatchingTransport(t *testing.T) {
	sess := newSession(zaptest.NewLogger(t), "127.0.0.1:1", nil, nil)

	sess.mu.Lock()
	sess.state = StateAppConnected
	sess.app = ReceiverApplication{AppID: DefaultMediaReceiverAppID, TransportID: "dmr-1"}
	sess.appTransportID = "app-old" // still the previous app's connection
	assert.False(t, sess.connectedToAppLocked(DefaultMediaReceiverAppID))
	sess.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := sess.awaitApp(ctx, DefaultMediaReceiverAppID, 0)
	require.ErrorIs(t, err, ErrNoActiveApp, "must not hand back the old app's transport")

	sess.mu.Lock()
	sess.appTransportID = "dmr-1"
	assert.True(t, sess.connectedToAppLocked(DefaultMediaReceiverAppID))
	sess.mu.Unlock()

	got, err := sess.awaitApp(context.Background(), DefaultMediaReceiverAppID, 0)
	require.NoError(t, err)
	assert.Equal(t, "dmr-1", got)
}

// TestSession_NonStatusReceiverMessagesDoNotWipeState verifies receiver-
// namespace messages that are not a RECEIVER_STATUS leave the cached app and
// volume alone. Decoded as a status they look like an empty one.
func TestSession_NonStatusReceiverMessagesDoNotWipeState(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: "CAFE0001", TransportID: "app-old"}},
		Volume:       ReceiverVolume{Level: 0.4, ControlType: "master", StepInterval: 0.02},
	})
	sd.setMediaStatus("app-old", []MediaStatus{{MediaSessionID: 7, PlayerState: "PLAYING"}})

	sess, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		st := rec.get()
		return st.State == StateAppConnected && st.Volume.Level == 0.4
	}, 2*time.Second, 10*time.Millisecond)

	sd.pushLaunchError("NOT_FOUND")
	_ = sd.fd.send(sd.launchStatusMessage(0))
	_ = sd.fd.send(sd.errorMessage(NamespaceReceiver, 0, errorPayload{Type: TypeInvalidRequest, Reason: "INVALID_COMMAND"}))

	// A later, well-formed status proves the messages above were processed
	// (the connection delivers in order) before we look.
	sd.pushReceiverStatus(receiverStatus{
		Applications: []ReceiverApplication{{AppID: "CAFE0001", TransportID: "app-old"}},
		Volume:       ReceiverVolume{Level: 0.5, ControlType: "master", StepInterval: 0.02},
	})
	require.Eventually(t, func() bool { return sess.Status().Volume.Level == 0.5 }, 2*time.Second, 10*time.Millisecond)

	st := sess.Status()
	assert.Equal(t, "CAFE0001", st.App.AppID, "app must survive non-status messages")
	assert.Equal(t, StateAppConnected, st.State)
	for _, v := range rec.levels() {
		assert.NotZero(t, v, "volume must never have been published as zero")
	}
}

// TestSession_LoadMedia_AbortsOnLaunchError verifies a launch failure the
// device reports outside any request (after a LAUNCH_STATUS used up the reply)
// ends LoadMedia at once as a rejection, rather than after its full deadline.
func TestSession_LoadMedia_AbortsOnLaunchError(t *testing.T) {
	sd := newScriptedDevice(t)
	sd.setReceiverStatus(receiverStatus{})
	sd.setLaunchAsStatus(true)

	sess, rec := runSession(t, sd)
	require.Eventually(t, func() bool {
		return rec.get().State == StateReceiverConnected
	}, 2*time.Second, 10*time.Millisecond)

	go func() {
		time.Sleep(100 * time.Millisecond)
		sd.pushLaunchError("NOT_FOUND")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := sess.LoadMedia(ctx, LoadRequest{ContentURL: "https://example.com/a.mp3", ContentType: "audio/mp3"})

	require.ErrorIs(t, err, ErrRequestRejected)
	assert.Less(t, time.Since(start), 2*time.Second, "must not wait out the deadline")
	assert.Empty(t, sd.sentMediaPayloads(t, TypeLoad))
}
