package castctrl

// CASTV2 namespaces. Each is its own multiplexing channel over the same TLS
// connection; a CastMessage's Namespace field selects which one a payload
// belongs to.
const (
	NamespaceConnection = "urn:x-cast:com.google.cast.tp.connection"
	NamespaceHeartbeat  = "urn:x-cast:com.google.cast.tp.heartbeat"
	NamespaceReceiver   = "urn:x-cast:com.google.cast.receiver"
	NamespaceMedia      = "urn:x-cast:com.google.cast.media"
)

// Well-known source/destination endpoint IDs.
const (
	// SenderID is the source_id this library identifies itself as.
	SenderID = "sender-0"
	// ReceiverID is the destination_id of the device's platform (receiver)
	// endpoint, as opposed to a running application's transportId.
	ReceiverID = "receiver-0"
)

// JSON "type" values used across namespaces. Not every type Cast defines is
// listed here — only the ones this library sends or expects to receive.
const (
	TypeConnect = "CONNECT"
	TypeClose   = "CLOSE"

	TypePing = "PING"
	TypePong = "PONG"

	TypeGetStatus      = "GET_STATUS"
	TypeReceiverStatus = "RECEIVER_STATUS"
	TypeSetVolume      = "SET_VOLUME"
	TypeLaunch         = "LAUNCH"

	TypeMediaStatus = "MEDIA_STATUS"
	TypeLoad        = "LOAD"
	TypePlay        = "PLAY"
	TypePause       = "PAUSE"
	TypeStop        = "STOP"
	TypeSeek        = "SEEK"

	// TypeInvalidRequest, TypeLoadFailed, and TypeLaunchError are error
	// responses the device may send in place of the expected status type;
	// callers correlating by requestId should treat any of these as a
	// terminal (failed) response. TypeLaunchError is LAUNCH's own dedicated
	// error type, distinct from the generic TypeInvalidRequest.
	TypeInvalidRequest = "INVALID_REQUEST"
	TypeLoadFailed     = "LOAD_FAILED"
	TypeLaunchError    = "LAUNCH_ERROR"
)

// supportedMediaCommands bitmask values, per
// developers.google.com/cast/docs/media/messages.
const (
	MediaCommandPause        = 1 << 0
	MediaCommandSeek         = 1 << 1
	MediaCommandStreamVolume = 1 << 2
	MediaCommandStreamMute   = 1 << 3
	MediaCommandSkipForward  = 1 << 4
	MediaCommandSkipBackward = 1 << 5
)
