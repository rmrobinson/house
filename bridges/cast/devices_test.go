package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/bridges/lib/castctrl"
)

func TestDeviceKind(t *testing.T) {
	tests := []struct {
		name string
		txt  map[string]string
		want Kind
	}{
		{"home mini", map[string]string{"ca": "199172"}, KindMediaPlayer},
		{"chromecast audio", map[string]string{"ca": "199172"}, KindMediaPlayer},
		{"tv streamer", map[string]string{"ca": "465413"}, KindTelevision},
		{"missing ca", map[string]string{}, KindMediaPlayer},
		{"unparseable ca", map[string]string{"ca": "not-a-number"}, KindMediaPlayer},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, DeviceKind(tt.txt))
		})
	}
}

func TestMapPlaybackState(t *testing.T) {
	tests := []struct {
		name         string
		playerState  string
		idleReason   string
		playbackRate float64
		want         trait.Media_PlaybackState
	}{
		{"playing", "PLAYING", "", 1, trait.Media_PS_PLAYING},
		{"fast forward", "PLAYING", "", 2, trait.Media_PS_FAST_FORWARD},
		{"rewind", "PLAYING", "", -1, trait.Media_PS_REWIND},
		{"paused", "PAUSED", "", 1, trait.Media_PS_PAUSED},
		{"buffering", "BUFFERING", "", 1, trait.Media_PS_BUFFERING},
		{"idle finished", "IDLE", "FINISHED", 1, trait.Media_PS_COMPLETED},
		{"idle interrupted", "IDLE", "INTERRUPTED", 1, trait.Media_PS_STOPPED},
		{"idle no reason", "IDLE", "", 1, trait.Media_PS_STOPPED},
		{"unknown", "SOMETHING_ELSE", "", 1, trait.Media_PS_UNSPECIFIED},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mapPlaybackState(tt.playerState, tt.idleReason, tt.playbackRate))
		})
	}
}

func TestBuildMediaAttributes(t *testing.T) {
	// Confirmed from phase 1 recon: Spotify on the Kitchen Home Mini reported
	// supportedMediaCommands 1039823 — bits 0-3 set (pause, seek, stream
	// volume, stream mute), bits 4-5 clear (deprecated skip forward/back).
	attrs := buildMediaAttributes(1039823)
	assert.True(t, attrs.CanControl)
	assert.True(t, attrs.GetCanPause())
	assert.True(t, attrs.GetCanSeek())
	assert.False(t, attrs.GetCanSkipForward())
	assert.False(t, attrs.GetCanSkipBackward())

	none := buildMediaAttributes(0)
	assert.False(t, none.CanControl)
	assert.False(t, none.GetCanPause())
}

func TestBuildVolumeTrait(t *testing.T) {
	tests := []struct {
		name           string
		v              castctrl.ReceiverVolume
		wantMaxLevel   int32
		wantLevel      int32
		wantCanControl bool
	}{
		{
			name:           "master controlType still reports can_control (step-synthesis fallback in command.go)",
			v:              castctrl.ReceiverVolume{Level: 0.5, ControlType: "master", StepInterval: 0.02},
			wantMaxLevel:   50,
			wantLevel:      25,
			wantCanControl: true,
		},
		{
			name:           "attenuation can_control",
			v:              castctrl.ReceiverVolume{Level: 1.0, ControlType: "attenuation", StepInterval: 0.1},
			wantMaxLevel:   10,
			wantLevel:      10,
			wantCanControl: true,
		},
		{
			name:           "fixed cannot control",
			v:              castctrl.ReceiverVolume{Level: 0.8, ControlType: "fixed", StepInterval: 0.1},
			wantMaxLevel:   10,
			wantLevel:      8,
			wantCanControl: false,
		},
		{
			name:           "stepInterval that does not divide evenly into 1.0",
			v:              castctrl.ReceiverVolume{Level: 0.3, ControlType: "attenuation", StepInterval: 0.03},
			wantMaxLevel:   33, // round(1/0.03) = round(33.33) = 33
			wantLevel:      10, // round(0.3*33) = round(9.9) = 10
			wantCanControl: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vol := buildVolumeTrait(tt.v)
			assert.Equal(t, tt.wantMaxLevel, vol.Attributes.MaximumLevel)
			assert.Equal(t, tt.wantLevel, vol.State.Level)
			assert.Equal(t, tt.wantCanControl, vol.Attributes.CanControl)
			assert.True(t, vol.Attributes.CanMute)
		})
	}
}

func TestApplyMediaDetails(t *testing.T) {
	t.Run("metadataType 0 generic", func(t *testing.T) {
		state := &trait.Media_State{}
		applyMediaDetails(state, &castctrl.MediaMetadata{
			MetadataType: 0,
			Title:        "Morning News",
			Subtitle:     "NPR",
		})
		assert.Equal(t, trait.Media_TYPE_GENERIC, state.MediaType)
		require.NotNil(t, state.GenericDetails)
		assert.Equal(t, "Morning News", state.GenericDetails.Title)
		assert.Equal(t, "NPR", state.GenericDetails.Subtitle)
	})

	t.Run("metadataType 1 movie", func(t *testing.T) {
		state := &trait.Media_State{}
		applyMediaDetails(state, &castctrl.MediaMetadata{
			MetadataType: 1,
			Title:        "Some Movie",
			Studio:       "A Studio",
			ReleaseDate:  "1999-03-31",
		})
		assert.Equal(t, trait.Media_TYPE_MOVIE, state.MediaType)
		require.NotNil(t, state.MovieDetails)
		assert.Equal(t, "Some Movie", state.MovieDetails.Title)
		assert.Equal(t, int32(1999), state.MovieDetails.ReleaseYear)
	})

	t.Run("metadataType 2 tv show", func(t *testing.T) {
		state := &trait.Media_State{}
		applyMediaDetails(state, &castctrl.MediaMetadata{
			MetadataType: 2,
			SeriesTitle:  "A Show",
			EpisodeTitle: "Pilot",
		})
		assert.Equal(t, trait.Media_TYPE_SHOW, state.MediaType)
		require.NotNil(t, state.ShowDetails)
		assert.Equal(t, "A Show", state.ShowDetails.ShowTitle)
		assert.Equal(t, "Pilot", state.ShowDetails.EpisodeTitle)
	})

	t.Run("metadataType 3 music track", func(t *testing.T) {
		state := &trait.Media_State{}
		applyMediaDetails(state, &castctrl.MediaMetadata{
			MetadataType: 3,
			Title:        "A Song",
			Artist:       "An Artist",
			AlbumName:    "An Album",
			ReleaseDate:  "2020",
			Images: []struct {
				URL string `json:"url"`
			}{{URL: "http://example.com/art.jpg"}},
		})
		assert.Equal(t, trait.Media_TYPE_SONG, state.MediaType)
		require.NotNil(t, state.SongDetails)
		assert.Equal(t, "A Song", state.SongDetails.SongName)
		assert.Equal(t, []string{"An Artist"}, state.SongDetails.Artists)
		assert.Equal(t, "An Album", state.SongDetails.AlbumName)
		assert.Equal(t, int32(2020), state.SongDetails.ReleaseYear)
		require.NotNil(t, state.SongDetails.AlbumArtUrl)
		assert.Equal(t, "http://example.com/art.jpg", *state.SongDetails.AlbumArtUrl)
	})

	t.Run("metadataType 4 photo lands as generic", func(t *testing.T) {
		state := &trait.Media_State{}
		applyMediaDetails(state, &castctrl.MediaMetadata{MetadataType: 4, Title: "A Photo"})
		assert.Equal(t, trait.Media_TYPE_GENERIC, state.MediaType)
		require.NotNil(t, state.GenericDetails)
	})

	t.Run("nil metadata leaves state untouched", func(t *testing.T) {
		state := &trait.Media_State{}
		applyMediaDetails(state, nil)
		assert.Equal(t, trait.Media_TYPE_UNSPECIFIED, state.MediaType)
		assert.Nil(t, state.GenericDetails)
	})
}

func TestReleaseYear(t *testing.T) {
	assert.Equal(t, int32(1999), releaseYear("1999-03-31"))
	assert.Equal(t, int32(2020), releaseYear("2020"))
	assert.Equal(t, int32(0), releaseYear(""))
	assert.Equal(t, int32(0), releaseYear("abc"))
}

func TestNormalize_MediaPlayerVsTelevision(t *testing.T) {
	now := time.Now()
	status := castctrl.Status{State: castctrl.StateReceiverConnected}

	mp := Normalize(now, NormalizeOptions{DeviceID: "id1", Kind: KindMediaPlayer, Name: "Kitchen Speaker"}, status)
	assert.NotNil(t, mp.GetMediaPlayer())
	assert.Nil(t, mp.GetTelevision())
	assert.Equal(t, "Kitchen Speaker", mp.GetConfig().GetName())

	tv := Normalize(now, NormalizeOptions{DeviceID: "id2", Kind: KindTelevision, Name: "Living Room TV"}, status)
	assert.NotNil(t, tv.GetTelevision())
	assert.Nil(t, tv.GetMediaPlayer())
}

func TestNormalize_Reachability(t *testing.T) {
	now := time.Now()

	connected := Normalize(now, NormalizeOptions{DeviceID: "id1"}, castctrl.Status{State: castctrl.StateAppConnected})
	assert.True(t, connected.GetAddress().GetIsReachable())

	disconnected := Normalize(now, NormalizeOptions{DeviceID: "id1"}, castctrl.Status{State: castctrl.StateDisconnected})
	assert.False(t, disconnected.GetAddress().GetIsReachable())
}

func TestNormalize_NoMediaIsInactive(t *testing.T) {
	now := time.Now()
	d := Normalize(now, NormalizeOptions{DeviceID: "id1", Kind: KindMediaPlayer}, castctrl.Status{
		State:    castctrl.StateReceiverConnected,
		HasMedia: false,
	})
	assert.Equal(t, trait.Media_DEVICE_STATE_INACTIVE, d.GetMediaPlayer().GetMedia().GetState().GetDeviceState())
}
