package main

import (
	"context"
	"errors"
	"math"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/bridges/lib/castctrl"
)

// commandTimeout bounds how long a single-round-trip command waits for the
// device's correlated response before giving up, per B8's "5s suggested"
// guidance.
const commandTimeout = 5 * time.Second

// perStepTimeout bounds a single step's round trip within a step-synthesized
// volume change (see setVolumeAbsolute) — a "master" controlType device
// honors absolute sets unreliably, so an absolute VolumeAbsolute/
// VolumeRelative there is carried out as a sequence of single-step SET_VOLUME
// calls. commandTimeout alone is sized for one round trip; reusing it as the
// deadline for the whole sequence means any jump of more than a handful of
// steps would almost always time out partway through, leaving the device at
// an unpredictable level. commandDeadline scales the overall deadline by
// step count instead, and each step is additionally capped at
// perStepTimeout so one stuck step can't silently consume the whole budget.
const perStepTimeout = 2 * time.Second

// executeCommand dispatches cmd against cd's session and returns the
// resulting normalized device. castctrl.Session's write methods already
// block for the device's correlated response (bounded by ctx), so this same
// function backs both ProcessCommand (synchronous) and ProcessCommandAsync
// (backgrounded by the caller) — the two RPCs differ only in how the caller
// is notified of the outcome, not in how the command itself is carried out.
func (cb *CastBridge) executeCommand(ctx context.Context, cd *castDevice, cmd *command.Command) (*device.Device, error) {
	if err := dispatchCommand(ctx, cd, cmd); err != nil {
		return nil, err
	}
	return cb.currentDevice(cd), nil
}

func (cb *CastBridge) currentDevice(cd *castDevice) *device.Device {
	return cb.normalizeCurrent(cd, cd.session.Status())
}

func dispatchCommand(ctx context.Context, cd *castDevice, cmd *command.Command) error {
	sess := cd.session

	switch {
	case cmd.GetPlayback() != nil:
		switch cmd.GetPlayback().GetAction() {
		case command.Playback_ACTION_PLAY:
			return translateErr(sess.Play(ctx))
		case command.Playback_ACTION_PAUSE:
			return translateErr(sess.Pause(ctx))
		case command.Playback_ACTION_STOP:
			return translateErr(sess.StopMedia(ctx))
		default:
			return status.Error(codes.InvalidArgument, "unspecified playback action")
		}

	case cmd.GetSeekAbsolute() != nil:
		// Unlike SeekRelative below, PositionS here is passed through
		// unclamped — a caller-supplied SeekAbsolute is assumed to already be
		// a valid position for the current track (e.g. computed from a
		// client's own UI, which has PlaybackLengthS to bound against).
		// castctrl.Session.SeekAbsolute's doc comment has the real-hardware
		// consequence of not doing so: an out-of-range position isn't
		// rejected or clamped by the device, it silently skips to a different
		// track in the queue.
		return translateErr(sess.SeekAbsolute(ctx, cmd.GetSeekAbsolute().GetPositionS()))

	case cmd.GetSeekRelative() != nil:
		st := sess.Status()
		target := st.Media.CurrentTime + cmd.GetSeekRelative().GetDeltaS()
		if target < 0 {
			target = 0
		}
		if st.Media.Media != nil && st.Media.Media.Duration > 0 && target > st.Media.Media.Duration {
			target = st.Media.Media.Duration
		}
		return translateErr(sess.SeekAbsolute(ctx, target))

	case cmd.GetVolumeAbsolute() != nil:
		return setVolumeAbsolute(ctx, cd, cmd.GetVolumeAbsolute().GetLevel())

	case cmd.GetVolumeRelative() != nil:
		st := sess.Status()
		maxLevel := maximumLevel(st.Volume.StepInterval)
		currentLevel := int32(math.Round(st.Volume.Level * float64(maxLevel)))
		return setVolumeAbsolute(ctx, cd, currentLevel+cmd.GetVolumeRelative().GetDelta())

	case cmd.GetMute() != nil:
		return translateErr(sess.SetMuted(ctx, cmd.GetMute().GetIsMuted()))

	case cmd.GetAppLaunch() != nil:
		return translateErr(sess.LaunchApp(ctx, cmd.GetAppLaunch().GetApplicationId()))

	default:
		return status.Error(codes.InvalidArgument, "unrecognized command")
	}
}

// setVolumeAbsolute sets the device to targetLevel, an integer step count in
// trait.Volume.State.level's domain (i.e. already multiplied by
// maximumLevel, not a raw [0,1] fraction).
//
// On "attenuation" devices this is a single SET_VOLUME. On "master" devices,
// per the resolved step-synthesis decision (B4 in the handoff doc — absolute
// set isn't honored reliably on this controlType), it's issued as a sequence
// of single-step SET_VOLUME calls computed from the last-known level. This is
// fragile if the level changes concurrently from another sender (phone,
// voice) between read and write, since every step's target is computed once
// up front from a single snapshot rather than re-read between steps — but it
// gives working absolute control on hardware that doesn't support it
// directly, which was the resolved trade-off.
func setVolumeAbsolute(ctx context.Context, cd *castDevice, targetLevel int32) error {
	sess := cd.session
	st := sess.Status()
	maxLevel := maximumLevel(st.Volume.StepInterval)
	targetLevel = clampLevel(targetLevel, maxLevel)

	if st.Volume.ControlType != "master" {
		return translateErr(sess.SetVolumeLevel(ctx, float64(targetLevel)/float64(maxLevel)))
	}

	currentLevel := int32(math.Round(st.Volume.Level * float64(maxLevel)))
	step := int32(1)
	if targetLevel < currentLevel {
		step = -1
	}

	level := currentLevel
	for level != targetLevel {
		level += step

		stepErr := func() error {
			stepCtx, cancel := context.WithTimeout(ctx, perStepTimeout)
			defer cancel()
			return sess.SetVolumeLevel(stepCtx, float64(level)/float64(maxLevel))
		}()
		if stepErr != nil {
			return translateErr(stepErr)
		}
	}
	return nil
}

// commandDeadline returns how long cmd should be allowed to run against cd:
// commandTimeout for anything that resolves to a single round trip, or
// enough budget for every step a "master" controlType step-synthesis
// sequence will need, whichever is larger.
func commandDeadline(cd *castDevice, cmd *command.Command) time.Duration {
	steps := estimatedVolumeSteps(cd, cmd)
	if d := time.Duration(steps) * perStepTimeout; d > commandTimeout {
		return d
	}
	return commandTimeout
}

// estimatedVolumeSteps returns how many single-step SET_VOLUME round trips
// setVolumeAbsolute will issue for cmd against cd's current status, or 1 for
// anything else (a single round trip, or an absolute set on a non-"master"
// device, which is sent as one call regardless of distance).
func estimatedVolumeSteps(cd *castDevice, cmd *command.Command) int32 {
	st := cd.session.Status()
	if st.Volume.ControlType != "master" {
		return 1
	}

	maxLevel := maximumLevel(st.Volume.StepInterval)
	currentLevel := int32(math.Round(st.Volume.Level * float64(maxLevel)))

	var targetLevel int32
	switch {
	case cmd.GetVolumeAbsolute() != nil:
		targetLevel = clampLevel(cmd.GetVolumeAbsolute().GetLevel(), maxLevel)
	case cmd.GetVolumeRelative() != nil:
		targetLevel = clampLevel(currentLevel+cmd.GetVolumeRelative().GetDelta(), maxLevel)
	default:
		return 1
	}

	steps := targetLevel - currentLevel
	if steps < 0 {
		steps = -steps
	}
	if steps < 1 {
		return 1
	}
	return steps
}

func maximumLevel(stepInterval float64) int32 {
	if stepInterval <= 0 {
		return 1
	}
	return int32(math.Round(1 / stepInterval))
}

func clampLevel(level, maxLevel int32) int32 {
	switch {
	case level < 0:
		return 0
	case level > maxLevel:
		return maxLevel
	default:
		return level
	}
}

// translateErr maps a castctrl error into the gRPC status code B8 specifies:
// DEADLINE_EXCEEDED on timeout, FAILED_PRECONDITION when the device itself
// rejected the request (INVALID_REQUEST/LOAD_FAILED/LAUNCH_ERROR — retrying
// the identical command is expected to fail again, unlike a transient
// connection issue),
// and UNAVAILABLE for everything else a write can fail with here — no active
// app/media/receiver connection to target, or the underlying connection
// dropping mid-request. None of these are the caller's fault in the way an
// InvalidArgument would be, so UNAVAILABLE (retryable) is the fallback for
// anything not called out above rather than per-cause codes.
func translateErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, err.Error())
	}
	if errors.Is(err, castctrl.ErrRequestRejected) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return status.Error(codes.Unavailable, err.Error())
}
