package webosctrl

import "encoding/json"

// requestEnvelope is the generic message shape sent to the device: ssap://
// command requests, subscriptions, and the register handshake all use this
// same {type, id, uri, payload} shape.
type requestEnvelope struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	URI     string `json:"uri,omitempty"`
	Payload any    `json:"payload,omitempty"`
}

// envelope is what every inbound message decodes into first. Payload is
// left raw so the caller can decode it further once it knows which request
// (by ID) it's answering.
type envelope struct {
	Type    string          `json:"type"` // "response", "registered", "error"
	ID      string          `json:"id"`
	Error   string          `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// manifestPermissions is sent verbatim in every register request. Scoped to
// the three requested capabilities (volume, app launch, channel) plus the
// minimum read-back needed to populate Television state.
//
// This is deliberately narrow and not meant to grow casually: LG freezes an
// already-paired client-key to whatever permission set it was paired with,
// so widening this list later forces every previously-paired TV through a
// fresh on-screen accept prompt, not just a config change.
//
// LG does not publicly document the permission-to-endpoint mapping — this
// list is reverse-engineered from multiple independent open-source clients'
// manifests, not from LG source. Treat it as a strong starting point, not
// ground truth: if a specific endpoint 401s despite the "right" permission
// being present per community consensus, that's a real possibility, not
// necessarily a bridge bug (see the handoff/plan's recon item 1).
var manifestPermissions = []string{
	"CONTROL_AUDIO",
	"CONTROL_INPUT_TV",
	"READ_CURRENT_CHANNEL",
	"LAUNCH",
	"LAUNCH_WEBAPP",
	"CLOSE",
	"READ_RUNNING_APPS",
	"READ_APP_STATUS",
	"READ_INSTALLED_APPS",
	"CONTROL_POWER",
	"TEST_SECURE",
}

type registerPayload struct {
	ForcePairing bool             `json:"forcePairing"`
	PairingType  string           `json:"pairingType"`
	Manifest     registerManifest `json:"manifest"`
	ClientKey    string           `json:"client-key,omitempty"`
}

type registerManifest struct {
	ManifestVersion int      `json:"manifestVersion"`
	Permissions     []string `json:"permissions"`
}

// newRegisterPayload builds the register payload. clientKey is empty on
// first pairing (triggers the on-screen accept prompt) and set on every
// subsequent connect to skip it.
func newRegisterPayload(clientKey string) registerPayload {
	return registerPayload{
		PairingType: "PROMPT",
		Manifest: registerManifest{
			ManifestVersion: 1,
			Permissions:     manifestPermissions,
		},
		ClientKey: clientKey,
	}
}

// registeredPayload is the payload of a terminal "registered" response.
type registeredPayload struct {
	ClientKey string `json:"client-key"`
}

// returnValuePayload is the common {"returnValue": bool} shape most ssap://
// command responses use to signal success.
type returnValuePayload struct {
	ReturnValue bool   `json:"returnValue"`
	ErrorText   string `json:"errorText,omitempty"`
}

// The ssap:// URIs this client calls. Endpoint accuracy (which permission
// actually gates which URI, and whether these are the correct URIs at all
// on Robert's firmware) is unverified against a real device — see the
// handoff/plan's recon items.
const (
	uriGetVolume     = "ssap://audio/getVolume"
	uriSetVolume     = "ssap://audio/setVolume"
	uriSetMute       = "ssap://audio/setMute"
	uriGetPower      = "ssap://com.webos.service.tvpower/power/getPowerState"
	uriTurnOnScreen  = "ssap://com.webos.service.tvpower/power/turnOnScreen"
	uriTurnOffScreen = "ssap://com.webos.service.tvpower/power/turnOffScreen"
	uriSystemTurnOff = "ssap://system/turnOff"
	uriLaunchApp     = "ssap://system.launcher/launch"
	uriCloseApp      = "ssap://system.launcher/close"
	uriListApps      = "ssap://com.webos.applicationManager/listApps"
	uriForegroundApp = "ssap://com.webos.applicationManager/getForegroundAppInfo"
	uriGetChannel    = "ssap://tv/getCurrentChannel"
	uriOpenChannel   = "ssap://tv/openChannel"
	uriChannelUp     = "ssap://tv/channelUp"
	uriChannelDown   = "ssap://tv/channelDown"
)

type volumePayload struct {
	Volume int32 `json:"volume"`
	Muted  bool  `json:"muted"`
}

type powerPayload struct {
	// State is one of (per community-client precedent, unverified against a
	// real device): "Active", "Screen Saver", "Active Standby", "Suspend",
	// "Screen Off". Only "Active" and states containing "Screen" or "Standby"
	// are distinguished by ParsePowerState — see its doc comment.
	State string `json:"state"`
}

type appInfo struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

type listAppsPayload struct {
	Apps []appInfo `json:"apps"`
}

type foregroundAppPayload struct {
	AppID string `json:"appId"`
}

type launchAppPayload struct {
	ID string `json:"id"`
}

type channelPayload struct {
	ChannelID     string `json:"channelId"`
	ChannelNumber string `json:"channelNumber"`
	ChannelName   string `json:"channelName"`
}

type openChannelPayload struct {
	ChannelNumber string `json:"channelNumber"`
}

type setVolumePayload struct {
	Volume int32 `json:"volume"`
}

type setMutePayload struct {
	Mute bool `json:"mute"`
}
