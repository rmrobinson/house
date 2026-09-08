package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/rmrobinson/house/bridges/lib/castctrl"
)

func TestComputeVersion_UnchangedAcrossExcludedFields(t *testing.T) {
	base := Normalize(time.Now(), NormalizeOptions{DeviceID: "d1", Name: "Speaker"}, castctrl.Status{
		State:    castctrl.StateAppConnected,
		HasMedia: true,
		Media: castctrl.MediaStatus{
			PlayerState: "PLAYING",
			CurrentTime: 10,
			Media:       &castctrl.MediaInformation{Duration: 200},
		},
		Volume: castctrl.ReceiverVolume{Level: 0.5, ControlType: "attenuation", StepInterval: 0.1},
	})
	baseVersion := ComputeVersion(base)

	t.Run("playback_position_s change", func(t *testing.T) {
		later := Normalize(time.Now().Add(time.Minute), NormalizeOptions{DeviceID: "d1", Name: "Speaker"}, castctrl.Status{
			State:    castctrl.StateAppConnected,
			HasMedia: true,
			Media: castctrl.MediaStatus{
				PlayerState: "PLAYING",
				CurrentTime: 45, // position moved, as it does every tick of normal playback
				Media:       &castctrl.MediaInformation{Duration: 200},
			},
			Volume: castctrl.ReceiverVolume{Level: 0.5, ControlType: "attenuation", StepInterval: 0.1},
		})
		assert.Equal(t, baseVersion, ComputeVersion(later))
	})

	t.Run("last_seen change", func(t *testing.T) {
		later := Normalize(time.Now().Add(time.Hour), NormalizeOptions{DeviceID: "d1", Name: "Speaker"}, castctrl.Status{
			State:    castctrl.StateAppConnected,
			HasMedia: true,
			Media: castctrl.MediaStatus{
				PlayerState: "PLAYING",
				CurrentTime: 10,
				Media:       &castctrl.MediaInformation{Duration: 200},
			},
			Volume: castctrl.ReceiverVolume{Level: 0.5, ControlType: "attenuation", StepInterval: 0.1},
		})
		assert.Equal(t, baseVersion, ComputeVersion(later))
	})

	t.Run("metadata change with no command controlling it", func(t *testing.T) {
		later := Normalize(time.Now(), NormalizeOptions{DeviceID: "d1", Name: "Speaker"}, castctrl.Status{
			State:    castctrl.StateAppConnected,
			HasMedia: true,
			Media: castctrl.MediaStatus{
				PlayerState: "PLAYING",
				CurrentTime: 10,
				Media: &castctrl.MediaInformation{
					Duration: 200,
					Metadata: &castctrl.MediaMetadata{MetadataType: 3, Title: "A different song entirely"},
				},
			},
			Volume: castctrl.ReceiverVolume{Level: 0.5, ControlType: "attenuation", StepInterval: 0.1},
		})
		assert.Equal(t, baseVersion, ComputeVersion(later))
	})
}

func TestComputeVersion_ChangedAcrossIncludedFields(t *testing.T) {
	base := Normalize(time.Now(), NormalizeOptions{DeviceID: "d1", Name: "Speaker"}, castctrl.Status{
		State:    castctrl.StateAppConnected,
		HasMedia: true,
		Media:    castctrl.MediaStatus{PlayerState: "PLAYING"},
		Volume:   castctrl.ReceiverVolume{Level: 0.5, ControlType: "attenuation", StepInterval: 0.1},
	})
	baseVersion := ComputeVersion(base)

	t.Run("playback_state transition", func(t *testing.T) {
		paused := Normalize(time.Now(), NormalizeOptions{DeviceID: "d1", Name: "Speaker"}, castctrl.Status{
			State:    castctrl.StateAppConnected,
			HasMedia: true,
			Media:    castctrl.MediaStatus{PlayerState: "PAUSED"},
			Volume:   castctrl.ReceiverVolume{Level: 0.5, ControlType: "attenuation", StepInterval: 0.1},
		})
		assert.NotEqual(t, baseVersion, ComputeVersion(paused))
	})

	t.Run("volume level change", func(t *testing.T) {
		louder := Normalize(time.Now(), NormalizeOptions{DeviceID: "d1", Name: "Speaker"}, castctrl.Status{
			State:    castctrl.StateAppConnected,
			HasMedia: true,
			Media:    castctrl.MediaStatus{PlayerState: "PLAYING"},
			Volume:   castctrl.ReceiverVolume{Level: 0.9, ControlType: "attenuation", StepInterval: 0.1},
		})
		assert.NotEqual(t, baseVersion, ComputeVersion(louder))
	})

	t.Run("mute change", func(t *testing.T) {
		muted := Normalize(time.Now(), NormalizeOptions{DeviceID: "d1", Name: "Speaker"}, castctrl.Status{
			State:    castctrl.StateAppConnected,
			HasMedia: true,
			Media:    castctrl.MediaStatus{PlayerState: "PLAYING"},
			Volume:   castctrl.ReceiverVolume{Level: 0.5, Muted: true, ControlType: "attenuation", StepInterval: 0.1},
		})
		assert.NotEqual(t, baseVersion, ComputeVersion(muted))
	})

	t.Run("config name change", func(t *testing.T) {
		renamed := Normalize(time.Now(), NormalizeOptions{DeviceID: "d1", Name: "Different Name"}, castctrl.Status{
			State:    castctrl.StateAppConnected,
			HasMedia: true,
			Media:    castctrl.MediaStatus{PlayerState: "PLAYING"},
			Volume:   castctrl.ReceiverVolume{Level: 0.5, ControlType: "attenuation", StepInterval: 0.1},
		})
		assert.NotEqual(t, baseVersion, ComputeVersion(renamed))
	})
}
