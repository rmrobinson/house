package castctrl

// JSON payload bodies carried in CastMessage.payload_utf8. Cast namespaces
// exchange plain JSON, not protobuf — cast_channel.proto only frames the
// envelope. requestId correlates a response to the request that triggered
// it; 0 means the message is unsolicited (a push from the device, not a
// reply).
//
// Sub-structs that Session exposes as part of its public Status snapshot
// (ReceiverVolume, ReceiverApplication, MediaStatus, MediaInformation,
// MediaMetadata) are exported directly rather than duplicated into a
// parallel set of types — they're already a reasonable public shape, and
// castctrl has no house/proto dependency pulling in a competing one.

// envelope is embedded in every payload solely to read the "type" field
// during initial dispatch, before decoding into the type-specific struct.
type envelope struct {
	Type      string `json:"type"`
	RequestID uint32 `json:"requestId"`
}

// connection namespace

type connectPayload struct {
	Type string `json:"type"`
}

type closePayload struct {
	Type string `json:"type"`
}

// heartbeat namespace

type pingPayload struct {
	Type string `json:"type"`
}

type pongPayload struct {
	Type string `json:"type"`
}

// receiver namespace

type getStatusPayload struct {
	Type      string `json:"type"`
	RequestID uint32 `json:"requestId"`
}

type receiverStatusPayload struct {
	Type      string         `json:"type"`
	RequestID uint32         `json:"requestId"`
	Status    receiverStatus `json:"status"`
}

type receiverStatus struct {
	Applications []ReceiverApplication `json:"applications"`
	Volume       ReceiverVolume        `json:"volume"`
}

// ReceiverApplication is the currently-running application, as reported by
// RECEIVER_STATUS. Zero value means no application is running.
type ReceiverApplication struct {
	AppID       string `json:"appId"`
	DisplayName string `json:"displayName"`
	StatusText  string `json:"statusText"`
	TransportID string `json:"transportId"`
	Namespaces  []struct {
		Name string `json:"name"`
	} `json:"namespaces"`
}

// ReceiverVolume mirrors receiver.volume as reported by RECEIVER_STATUS.
// ControlType is one of "attenuation" (absolute set works), "fixed" (no
// controllable volume), or "master" (only relative step is reliable) — see
// castctrl's callers for how each is handled.
type ReceiverVolume struct {
	Level        float64 `json:"level"`
	Muted        bool    `json:"muted"`
	ControlType  string  `json:"controlType"`
	StepInterval float64 `json:"stepInterval"`
}

type setVolumePayload struct {
	Type      string          `json:"type"`
	RequestID uint32          `json:"requestId"`
	Volume    setVolumeValues `json:"volume"`
}

// setVolumeValues is intentionally partial: send only Level, or only Muted,
// per SET_VOLUME command; leave the other pointer nil so it's omitted rather
// than clobbering the value the caller didn't mean to change.
type setVolumeValues struct {
	Level *float64 `json:"level,omitempty"`
	Muted *bool    `json:"muted,omitempty"`
}

type launchPayload struct {
	Type      string `json:"type"`
	RequestID uint32 `json:"requestId"`
	AppID     string `json:"appId"`
}

// media namespace

// mediaStatusPayload wraps MEDIA_STATUS, both as a GET_STATUS response and as
// the unsolicited push the device sends on every playback change. Status is
// an array because Cast supports concurrent media sessions in principle;
// callers use Status[0] since this bridge only tracks the single active
// session an AppConnected session implies.
type mediaStatusPayload struct {
	Type      string        `json:"type"`
	RequestID uint32        `json:"requestId"`
	Status    []MediaStatus `json:"status"`
}

// MediaStatus is one entry of a MEDIA_STATUS push or GET_STATUS response.
// Zero value (via Session.Status().HasMedia == false) means no media session.
type MediaStatus struct {
	MediaSessionID         int               `json:"mediaSessionId"`
	PlayerState            string            `json:"playerState"` // IDLE, PLAYING, PAUSED, BUFFERING
	IdleReason             string            `json:"idleReason,omitempty"`
	CurrentTime            float64           `json:"currentTime"`
	PlaybackRate           float64           `json:"playbackRate"`
	SupportedMediaCommands int64             `json:"supportedMediaCommands"`
	Media                  *MediaInformation `json:"media,omitempty"`
}

// MediaInformation is the content-identity part of a MediaStatus.
type MediaInformation struct {
	ContentID  string         `json:"contentId"`
	StreamType string         `json:"streamType"`
	Duration   float64        `json:"duration"`
	Metadata   *MediaMetadata `json:"metadata,omitempty"`
}

// MediaMetadata is Cast's single flat metadata object; MetadataType selects
// which of the type-specific fields below are meaningfully populated. Not a
// discriminated wrapper on the wire — every sending app just sets whichever
// fields apply and leaves the rest zero.
type MediaMetadata struct {
	MetadataType int `json:"metadataType"`

	// Generic (0), Movie (1), TvShow (2)
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	Images   []struct {
		URL string `json:"url"`
	} `json:"images,omitempty"`

	// Movie (1)
	Studio string `json:"studio,omitempty"`

	// TvShow (2)
	SeriesTitle  string `json:"seriesTitle,omitempty"`
	Season       int    `json:"season,omitempty"`
	Episode      int    `json:"episode,omitempty"`
	EpisodeTitle string `json:"episodeTitle,omitempty"`

	// MusicTrack (3)
	Artist      string `json:"artist,omitempty"`
	AlbumName   string `json:"albumName,omitempty"`
	AlbumArtist string `json:"albumArtist,omitempty"`
	TrackNumber int    `json:"trackNumber,omitempty"`

	// Movie (1) and MusicTrack (3)
	ReleaseDate string `json:"releaseDate,omitempty"`
}

type playbackPayload struct {
	Type           string `json:"type"`
	RequestID      uint32 `json:"requestId"`
	MediaSessionID int    `json:"mediaSessionId"`
}

type seekPayload struct {
	Type           string   `json:"type"`
	RequestID      uint32   `json:"requestId"`
	MediaSessionID int      `json:"mediaSessionId"`
	CurrentTime    *float64 `json:"currentTime,omitempty"`
}
