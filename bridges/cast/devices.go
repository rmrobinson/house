package main

import (
	"math"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/bridges/lib/castctrl"
)

// Kind is the device shape a Cast endpoint should be normalized into.
type Kind int

const (
	KindMediaPlayer Kind = iota
	KindTelevision
)

// DeviceKind reports whether a discovered Cast device drives a video sink,
// from bit 0 of the mDNS "ca" TXT record. Confirmed against Google Home Mini,
// Chromecast Audio, and Google TV Streamer hardware (bit 0 cleanly separates
// the two audio-only families from the one video-capable family); unconfirmed
// against device families outside that set (e.g. Chromecast dongle, Nest
// Hub) — re-verify before trusting this bit for a new family. Defaults to
// KindMediaPlayer if "ca" is missing or unparseable.
func DeviceKind(txt map[string]string) Kind {
	ca, err := strconv.Atoi(txt["ca"])
	if err != nil {
		return KindMediaPlayer
	}
	if ca&1 != 0 {
		return KindTelevision
	}
	return KindMediaPlayer
}

// ParseKind maps a config-file "kind" string to a Kind, defaulting to
// KindMediaPlayer for anything other than "television". Used because this
// bridge's config is static-IP-first (see cast.example.yaml) — mDNS discovery
// (the source DeviceKind reads from) is optional and not what most devices
// will be configured through.
func ParseKind(s string) Kind {
	if strings.EqualFold(s, "television") {
		return KindTelevision
	}
	return KindMediaPlayer
}

// NormalizeOptions carries the static identity fields the normalizer needs
// alongside the dynamic castctrl.Status — the parts that come from
// config/mDNS rather than the device's own runtime status.
type NormalizeOptions struct {
	DeviceID  string // Cast UUID
	Address   string // "ip:8009"
	Kind      Kind
	Name      string // config.name default (mDNS friendly name, or config override)
	ModelID   string
	ModelName string
}

// Normalize converts a Session's Status snapshot into a device.Device. It has
// no knowledge of castctrl's internals beyond the exported Status shape, and
// no side effects — Device.Version is computed separately (version.go) since
// it needs to compare against the previous device state, which this function
// doesn't have.
func Normalize(now time.Time, opts NormalizeOptions, status castctrl.Status) *device.Device {
	d := &device.Device{
		Id:           opts.DeviceID,
		Manufacturer: "Google Inc.",
		ModelId:      opts.ModelID,
		Address: &device.Device_Address{
			Address:     opts.Address,
			IsReachable: status.State != castctrl.StateDisconnected,
		},
		Config:   &device.Device_Config{Name: opts.Name},
		LastSeen: timestamppb.New(now),
	}
	if opts.ModelName != "" {
		d.ModelName = proto.String(opts.ModelName)
	}

	volume := buildVolumeTrait(status.Volume)
	app := buildAppTrait(status.App)
	media := buildMediaTrait(now, status.Media, status.HasMedia)

	if opts.Kind == KindTelevision {
		d.Details = &device.Device_Television{
			Television: &device.Television{
				Volume: volume,
				App:    app,
				Media:  media,
			},
		}
	} else {
		d.Details = &device.Device_MediaPlayer{
			MediaPlayer: &device.MediaPlayer{
				Volume: volume,
				App:    app,
				Media:  media,
			},
		}
	}

	return d
}

func buildVolumeTrait(v castctrl.ReceiverVolume) *trait.Volume {
	maxLevel := int32(1)
	if v.StepInterval > 0 {
		maxLevel = int32(math.Round(1 / v.StepInterval))
	}

	// canControl is true for "master" as well as "attenuation" — "master"
	// devices don't honor an arbitrary absolute SET_VOLUME reliably, but the
	// write path (command.go) synthesizes an absolute set from a sequence of
	// single-step SET_VOLUME calls for those devices, so control is still
	// possible from the UI's perspective. "fixed" has no volume path at all.
	canControl := v.ControlType == "attenuation" || v.ControlType == "master"

	return &trait.Volume{
		Attributes: &trait.Volume_Attributes{
			CanControl:   canControl,
			CanMute:      true,
			MaximumLevel: maxLevel,
		},
		State: &trait.Volume_State{
			IsMuted: v.Muted,
			Level:   int32(math.Round(v.Level * float64(maxLevel))),
		},
	}
}

func buildAppTrait(app castctrl.ReceiverApplication) *trait.App {
	state := &trait.App_State{
		ApplicationId: app.AppID,
	}
	if app.DisplayName != "" {
		state.ApplicationName = proto.String(app.DisplayName)
	}
	if app.StatusText != "" {
		state.ApplicationStatus = proto.String(app.StatusText)
	}

	return &trait.App{
		// Attributes.applications stays empty — Cast has no local API to
		// enumerate what a device could run, only what it's running now.
		// CanControl is true regardless: LAUNCH targets an arbitrary appId,
		// so control doesn't depend on being able to enumerate the catalog.
		Attributes: &trait.App_Attributes{CanControl: true},
		State:      state,
	}
}

func buildMediaTrait(now time.Time, ms castctrl.MediaStatus, hasMedia bool) *trait.Media {
	if !hasMedia {
		return &trait.Media{
			Attributes: &trait.Media_Attributes{},
			State: &trait.Media_State{
				DeviceState: trait.Media_DEVICE_STATE_INACTIVE,
			},
		}
	}

	state := &trait.Media_State{
		DeviceState:               trait.Media_DEVICE_STATE_ACTIVE,
		PlaybackState:             mapPlaybackState(ms.PlayerState, ms.IdleReason, ms.PlaybackRate),
		PlaybackPositionS:         ms.CurrentTime,
		PlaybackPositionUpdatedAt: timestamppb.New(now),
	}

	if ms.Media != nil {
		state.PlaybackLengthS = ms.Media.Duration
		applyMediaDetails(state, ms.Media.Metadata)
	}

	return &trait.Media{
		Attributes: buildMediaAttributes(ms.SupportedMediaCommands),
		State:      state,
	}
}

func mapPlaybackState(playerState, idleReason string, playbackRate float64) trait.Media_PlaybackState {
	switch playerState {
	case "PLAYING":
		switch {
		case playbackRate > 1:
			return trait.Media_PS_FAST_FORWARD
		case playbackRate < 0:
			return trait.Media_PS_REWIND
		default:
			return trait.Media_PS_PLAYING
		}
	case "PAUSED":
		return trait.Media_PS_PAUSED
	case "BUFFERING":
		return trait.Media_PS_BUFFERING
	case "IDLE":
		if idleReason == "FINISHED" {
			return trait.Media_PS_COMPLETED
		}
		return trait.Media_PS_STOPPED
	default:
		return trait.Media_PS_UNSPECIFIED
	}
}

func buildMediaAttributes(supportedMediaCommands int64) *trait.Media_Attributes {
	canPause := supportedMediaCommands&castctrl.MediaCommandPause != 0
	canSeek := supportedMediaCommands&castctrl.MediaCommandSeek != 0
	canSkipForward := supportedMediaCommands&castctrl.MediaCommandSkipForward != 0
	canSkipBackward := supportedMediaCommands&castctrl.MediaCommandSkipBackward != 0

	return &trait.Media_Attributes{
		CanControl:      canPause || canSeek || canSkipForward || canSkipBackward,
		CanPause:        proto.Bool(canPause),
		CanSeek:         proto.Bool(canSeek),
		CanSkipForward:  proto.Bool(canSkipForward),
		CanSkipBackward: proto.Bool(canSkipBackward),
	}
}

// applyMediaDetails maps Cast's flat metadata object onto exactly one of
// State's four optional *Details fields, per metadataType. Every metadata
// field is populated on a best-effort basis — quality depends entirely on
// the sending app, and most fields are legitimately absent.
func applyMediaDetails(state *trait.Media_State, md *castctrl.MediaMetadata) {
	if md == nil {
		return
	}

	var artURL string
	if len(md.Images) > 0 {
		artURL = md.Images[0].URL
	}

	switch md.MetadataType {
	case 1: // Movie
		state.MediaType = trait.Media_TYPE_MOVIE
		state.MovieDetails = &trait.Media_MovieDetails{
			Title:       md.Title,
			Studio:      md.Studio,
			ReleaseYear: releaseYear(md.ReleaseDate),
			ArtUrl:      optionalString(artURL),
		}
	case 2: // TvShow
		state.MediaType = trait.Media_TYPE_SHOW
		state.ShowDetails = &trait.Media_ShowDetails{
			EpisodeTitle: md.EpisodeTitle,
			ShowTitle:    md.SeriesTitle,
			ArtUrl:       optionalString(artURL),
		}
	case 3: // MusicTrack
		state.MediaType = trait.Media_TYPE_SONG
		songDetails := &trait.Media_SongDetails{
			SongName:    md.Title,
			AlbumName:   md.AlbumName,
			ReleaseYear: releaseYear(md.ReleaseDate),
			AlbumArtUrl: optionalString(artURL),
		}
		if md.Artist != "" {
			songDetails.Artists = []string{md.Artist}
		}
		state.SongDetails = songDetails
	default:
		// Generic (0), Photo (4, ambient display isn't playback in any
		// useful sense), and any metadataType this mapping doesn't
		// recognize yet all land here rather than as TYPE_UNSPECIFIED with
		// nothing for the UI to render.
		state.MediaType = trait.Media_TYPE_GENERIC
		state.GenericDetails = &trait.Media_GenericDetails{
			Title:    md.Title,
			Subtitle: md.Subtitle,
			ArtUrl:   optionalString(artURL),
		}
	}
}

func releaseYear(releaseDate string) int32 {
	if len(releaseDate) < 4 {
		return 0
	}
	year, err := strconv.Atoi(releaseDate[:4])
	if err != nil {
		return 0
	}
	return int32(year)
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
