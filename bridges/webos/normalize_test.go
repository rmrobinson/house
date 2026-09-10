package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rmrobinson/house/bridges/lib/webosctrl"
)

func TestNormalize_NeverPaired_TraitsLeftNil(t *testing.T) {
	d := normalize(deviceConfig{UUID: "u1", Host: "192.168.1.42"}, webosctrl.Status{Reachable: true})

	tv := d.GetTelevision()
	require.NotNil(t, tv)
	assert.Nil(t, tv.GetOnOff())
	assert.Nil(t, tv.GetVolume())
	assert.Nil(t, tv.GetApp())
	assert.Nil(t, tv.GetChannel())
	assert.True(t, d.GetAddress().GetIsReachable())
}

func TestNormalize_DisconnectedAfterPairing_KeepsLastKnownTraits(t *testing.T) {
	// A device that was paired and reporting state, then lost its
	// connection (e.g. full standby) - EverPaired stays true even though
	// the live Paired/Reachable bits have gone false. This is exactly the
	// state SetOnOff(true)'s Wake-on-LAN path needs OnOff to still be
	// populated for (see service/bridge/api.go's deviceSupportsCommand,
	// which requires GetOnOff() != nil to accept the command at all).
	st := webosctrl.Status{
		Reachable:  false,
		Paired:     false,
		EverPaired: true,
		Power:      webosctrl.PowerActive,
		Volume:     42,
		Muted:      true,
	}

	d := normalize(deviceConfig{UUID: "u1", Host: "192.168.1.42"}, st)

	tv := d.GetTelevision()
	require.NotNil(t, tv.GetOnOff(), "OnOff must survive a disconnect so the wake command remains reachable")
	assert.True(t, tv.GetOnOff().GetState().GetIsOn())
	require.NotNil(t, tv.GetVolume())
	assert.EqualValues(t, 42, tv.GetVolume().GetState().GetLevel())
	assert.True(t, tv.GetVolume().GetState().GetIsMuted())
	assert.False(t, d.GetAddress().GetIsReachable())
}

func TestNormalize_Channel_RequiresBothConfigAndLiveConfirmation(t *testing.T) {
	pairedStatus := webosctrl.Status{EverPaired: true}

	t.Run("has_tuner false", func(t *testing.T) {
		d := normalize(deviceConfig{HasTuner: false}, func() webosctrl.Status {
			st := pairedStatus
			st.HasChannel = true
			return st
		}())
		assert.Nil(t, d.GetTelevision().GetChannel())
	})

	t.Run("has_tuner true but no channel push received yet", func(t *testing.T) {
		d := normalize(deviceConfig{HasTuner: true}, pairedStatus)
		assert.Nil(t, d.GetTelevision().GetChannel(), "must not surface an empty ChannelDetails before real data arrives")
	})

	t.Run("has_tuner true and confirmed live", func(t *testing.T) {
		st := pairedStatus
		st.HasChannel = true
		st.Channel = webosctrl.Channel{ID: "1", Number: "4-1", Name: "CBC HD"}

		d := normalize(deviceConfig{HasTuner: true}, st)
		ch := d.GetTelevision().GetChannel()
		require.NotNil(t, ch)
		assert.Equal(t, "4-1", ch.GetState().GetCurrentChannel().GetNumber())
	})
}
