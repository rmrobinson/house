package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	monopamp "github.com/rmrobinson/monoprice-amp-go"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/service/bridge"
)

type fakeZone struct {
	state      monopamp.State
	refreshErr error
	setErr     error
	calls      []string
}

func (f *fakeZone) ID() string             { return "11" }
func (f *fakeZone) Refresh() error         { return f.refreshErr }
func (f *fakeZone) State() *monopamp.State { return &f.state }
func (f *fakeZone) record(c string, e error) error {
	f.calls = append(f.calls, c)
	return e
}
func (f *fakeZone) SetPower(on bool) error {
	if f.setErr == nil {
		f.state.IsOn = on
	}
	return f.record("power", f.setErr)
}
func (f *fakeZone) SetMute(on bool) error {
	if f.setErr == nil {
		f.state.IsMuteOn = on
	}
	return f.record("mute", f.setErr)
}
func (f *fakeZone) SetVolume(l int) error {
	if f.setErr == nil {
		f.state.Volume = l
	}
	return f.record("volume", f.setErr)
}
func (f *fakeZone) SetTreble(l int) error {
	if f.setErr == nil {
		f.state.Treble = l
	}
	return f.record("treble", f.setErr)
}
func (f *fakeZone) SetBass(l int) error {
	if f.setErr == nil {
		f.state.Bass = l
	}
	return f.record("bass", f.setErr)
}
func (f *fakeZone) SetBalance(l int) error {
	if f.setErr == nil {
		f.state.Balance = l
	}
	return f.record("balance", f.setErr)
}
func (f *fakeZone) SetSourceChannel(c int) error {
	if f.setErr == nil {
		f.state.SourceChannelID = c
	}
	return f.record("input", f.setErr)
}

type fakeAmp map[int]*fakeZone

func (a fakeAmp) Zone(id int) zone {
	if z, ok := a[id]; ok {
		return z
	}
	return nil
}

const testBridgeID = "amp"

// newTestBridge has speakers 1 (active), 2 (active) and 3 (inactive), and inputs 1 and 2 (active) and 3 (inactive).
// zones controls which of them the fake amp reports.
func newTestBridge(t *testing.T, zones fakeAmp) *MonopriceAmpBridge {
	t.Helper()
	viper.Set("bridge.id", testBridgeID)
	logger := zaptest.NewLogger(t)
	b := NewMonopriceAmpBridge(logger, bridge.NewService(logger), "unused.yaml", "/dev/null", 9600,
		[]inputDetails{{ID: 1, Name: "One", Active: true}, {ID: 2, Name: "Two", Active: true}, {ID: 3, Name: "Off", Active: false}},
		[]speakerDetails{
			{ID: 1, Name: "Kitchen", Description: "Kitchen ceiling", Active: true},
			{ID: 2, Active: true},
			{ID: 3, Name: "Off", Active: false},
		})
	b.amp = zones
	return b
}

func cmdFor(speaker string, c *command.Command) *command.Command {
	c.DeviceId = testBridgeID + ":" + speaker
	return c
}

func TestProcessCommandRejectsBadTargets(t *testing.T) {
	b := newTestBridge(t, fakeAmp{1: {}, 3: {}}) // zone 2 configured but not reported
	on := &command.Command_OnOff{OnOff: &command.OnOff{On: true}}

	for name, id := range map[string]string{
		"foreign prefix":   "other:1",
		"no prefix":        "1",
		"non-numeric":      testBridgeID + ":abc",
		"empty speaker":    testBridgeID + ":",
		"unconfigured":     testBridgeID + ":9",
		"zero":             testBridgeID + ":0",
		"inactive speaker": testBridgeID + ":3",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := b.ProcessCommand(context.Background(), &command.Command{DeviceId: id, Details: on})
			assert.Equal(t, bridge.ErrDeviceNotFound, err)
		})
	}

	t.Run("configured but not reported by amp", func(t *testing.T) {
		_, err := b.ProcessCommand(context.Background(), cmdFor("2", &command.Command{Details: on}))
		assert.Equal(t, ErrZoneUnavailable, err)
	})
}

func TestProcessCommandVolume(t *testing.T) {
	z := &fakeZone{state: monopamp.State{Volume: 10}}
	b := newTestBridge(t, fakeAmp{1: z})
	run := func(c *command.Command) (int32, error) {
		d, err := b.ProcessCommand(context.Background(), cmdFor("1", c))
		if err != nil {
			return 0, err
		}
		return d.GetAvReceiver().GetVolume().GetState().GetLevel(), nil
	}
	abs := func(l int32) *command.Command {
		return &command.Command{Details: &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: l}}}
	}
	rel := func(d int32) *command.Command {
		return &command.Command{Details: &command.Command_VolumeRelative{VolumeRelative: &command.VolumeRelative{Delta: d}}}
	}

	// Native range, no scaling: what's sent is what's reported back.
	lvl, err := run(abs(38))
	require.NoError(t, err)
	assert.Equal(t, int32(38), lvl)
	assert.Equal(t, 38, z.state.Volume)

	for _, bad := range []int32{-1, 39, 100} {
		_, err = run(abs(bad))
		assert.Equal(t, bridge.ErrArgumentNotSupportedByDevice, err, "level %d", bad)
	}
	assert.Equal(t, 38, z.state.Volume, "rejected commands must not reach the amp")

	// A single step moves a single step.
	z.state.Volume = 20
	lvl, _ = run(rel(1))
	assert.Equal(t, int32(21), lvl)
	lvl, _ = run(rel(-2))
	assert.Equal(t, int32(19), lvl)

	// Clamped at both ends.
	lvl, _ = run(rel(500))
	assert.Equal(t, int32(38), lvl)
	lvl, _ = run(rel(-500))
	assert.Equal(t, int32(0), lvl)

	d, err := b.ProcessCommand(context.Background(), cmdFor("1", &command.Command{Details: &command.Command_Mute{Mute: &command.Mute{IsMuted: true}}}))
	require.NoError(t, err)
	assert.True(t, d.GetAvReceiver().GetVolume().GetState().GetIsMuted())
	assert.Equal(t, int32(maxVolumeLevel), d.GetAvReceiver().GetVolume().GetAttributes().GetMaximumLevel())
}

func TestProcessCommandInput(t *testing.T) {
	z := &fakeZone{}
	b := newTestBridge(t, fakeAmp{1: z})
	in := func(id string) *command.Command {
		return &command.Command{Details: &command.Command_Input{Input: &command.Input{InputId: id}}}
	}

	d, err := b.ProcessCommand(context.Background(), cmdFor("1", in("2")))
	require.NoError(t, err)
	assert.Equal(t, "2", d.GetAvReceiver().GetInput().GetState().GetCurrentInputId())

	for _, bad := range []string{"3", "0", "x", ""} {
		_, err = b.ProcessCommand(context.Background(), cmdFor("1", in(bad)))
		assert.Equal(t, bridge.ErrArgumentNotSupportedByDevice, err, "input %q", bad)
	}
}

func TestProcessCommandAudioOutput(t *testing.T) {
	z := &fakeZone{}
	b := newTestBridge(t, fakeAmp{1: z})
	ptr := func(v int32) *int32 { return &v }
	ao := func(tr, ba, bal *int32) *command.Command {
		return &command.Command{Details: &command.Command_AudioOutput{AudioOutput: &command.AudioOutput{TrebleLevel: tr, BassLevel: ba, Balance: bal}}}
	}

	d, err := b.ProcessCommand(context.Background(), cmdFor("1", ao(ptr(14), ptr(0), ptr(20))))
	require.NoError(t, err)
	st := d.GetAvReceiver().GetAudioOutput().GetState()
	assert.Equal(t, int32(14), st.TrebleLevel)
	assert.Equal(t, int32(0), st.BassLevel)
	assert.Equal(t, int32(20), st.GetBalance())

	// Any out-of-range field rejects the whole command before anything is sent.
	z.calls = nil
	for name, c := range map[string]*command.Command{
		"treble high":  ao(ptr(15), nil, nil),
		"treble low":   ao(ptr(-1), nil, nil),
		"bass high":    ao(ptr(7), ptr(15), nil),
		"balance high": ao(ptr(7), ptr(7), ptr(21)),
		"balance low":  ao(nil, nil, ptr(-1)),
	} {
		_, err = b.ProcessCommand(context.Background(), cmdFor("1", c))
		assert.Equal(t, bridge.ErrArgumentNotSupportedByDevice, err, name)
	}
	assert.Empty(t, z.calls)

	// Failures writing to the amp are surfaced rather than swallowed.
	z.setErr = errors.New("serial down")
	_, err = b.ProcessCommand(context.Background(), cmdFor("1", ao(ptr(7), nil, nil)))
	assert.Equal(t, ErrSpeakerCommandFailed, err)
}

func TestDeviceDescribesSpeaker(t *testing.T) {
	b := newTestBridge(t, fakeAmp{1: {}, 2: {}})
	d, err := b.ProcessCommand(context.Background(), cmdFor("1", &command.Command{Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}}))
	require.NoError(t, err)
	assert.Equal(t, "Kitchen", d.GetConfig().GetName())
	assert.Equal(t, "Kitchen ceiling", d.GetConfig().GetDescription())
	assert.True(t, d.GetAddress().GetIsReachable())

	// Inactive input 3 isn't advertised.
	var inputIDs []string
	for _, in := range d.GetAvReceiver().GetInput().GetAttributes().GetInputs() {
		inputIDs = append(inputIDs, in.Id)
	}
	assert.Equal(t, []string{"1", "2"}, inputIDs)

	// No configured name falls back to something sensible.
	d, err = b.ProcessCommand(context.Background(), cmdFor("2", &command.Command{Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}}))
	require.NoError(t, err)
	assert.Equal(t, "Speaker 2", d.GetConfig().GetName())
}

func TestRefreshKeepsGoingPastAFailingZone(t *testing.T) {
	good := &fakeZone{state: monopamp.State{IsOn: true, Volume: 12}}
	bad := &fakeZone{refreshErr: errors.New("garbled response")}
	b := newTestBridge(t, fakeAmp{1: bad, 2: good})

	devices, err := b.refresh()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSpeakerRefreshFailed)
	assert.Contains(t, err.Error(), "garbled response")
	require.Len(t, devices, 2, "inactive speaker 3 isn't published; 1 and 2 both are")

	reachable := map[string]bool{}
	for _, d := range devices {
		reachable[d.Id] = d.GetAddress().GetIsReachable()
	}
	assert.False(t, reachable[testBridgeID+":1"])
	assert.True(t, reachable[testBridgeID+":2"])
	assert.True(t, b.bridgeReachable, "one zone still answering keeps the bridge reachable")

	// Recovery.
	bad.refreshErr = nil
	devices, err = b.refresh()
	require.NoError(t, err)
	for _, d := range devices {
		assert.True(t, d.GetAddress().GetIsReachable())
	}
}

func TestRefreshMissingZoneAndBridgeReachability(t *testing.T) {
	zones := fakeAmp{} // amp reported nothing
	b := newTestBridge(t, zones)

	devices, err := b.refresh()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrZoneUnavailable)
	require.Len(t, devices, 2, "unreported zones are still published, as unreachable")
	for _, d := range devices {
		assert.False(t, d.GetAddress().GetIsReachable())
	}
	assert.False(t, b.bridgeReachable)
	assert.False(t, b.b.IsReachable)

	zones[1] = &fakeZone{}
	_, err = b.refresh()
	assert.Error(t, err, "zone 2 is still missing")
	assert.True(t, b.bridgeReachable)
	assert.True(t, b.b.IsReachable)
}

// Run with -race: commands and refreshes arrive concurrently in real use.
func TestConcurrentCommandsAndRefresh(t *testing.T) {
	b := newTestBridge(t, fakeAmp{1: {}, 2: {}})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = b.ProcessCommand(context.Background(), cmdFor("1", &command.Command{Details: &command.Command_VolumeRelative{VolumeRelative: &command.VolumeRelative{Delta: 1}}}))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = b.Refresh(context.Background())
			}
		}()
	}
	wg.Wait()
}
