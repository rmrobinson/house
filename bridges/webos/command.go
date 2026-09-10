package main

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/bridges/lib/webosctrl"
)

// commandTimeout bounds how long a single-round-trip command waits for the
// device's correlated response.
const commandTimeout = 5 * time.Second

// webosSession is the subset of *webosctrl.Session that command dispatch
// needs. Defined as an interface (satisfied implicitly by *webosctrl.
// Session) so dispatchCommand can be tested against a fake without a live
// connection to a device - mirrors bridges/cast's castSession interface.
type webosSession interface {
	Status() webosctrl.Status
	TurnOnScreen(ctx context.Context) error
	TurnOffScreen(ctx context.Context) error
	PowerOff(ctx context.Context) error
	SetVolumeAbsolute(ctx context.Context, level int32) error
	SetMuted(ctx context.Context, muted bool) error
	LaunchApp(ctx context.Context, appID string) error
	CloseApp(ctx context.Context, appID string) error
	SetChannel(ctx context.Context, channelNumber string) error
	ChannelStep(ctx context.Context, delta int32) error
}

// wakeTimeout bounds how long SetOnOff(true) waits for the device to become
// reachable after a Wake-on-LAN packet, on top of commandTimeout for the
// TurnOnScreen call that follows.
const wakeTimeout = 20 * time.Second

// wakePollInterval is how often SetOnOff(true) checks Session.Status().Reachable
// while waiting for a device woken via WOL to come back online.
const wakePollInterval = 500 * time.Millisecond

// dispatchCommand executes cmd against wd's session.
func dispatchCommand(ctx context.Context, wd *webosDevice, cmd *command.Command) error {
	sess := wd.session

	switch {
	case cmd.GetOnOff() != nil:
		if cmd.GetOnOff().GetOn() {
			return turnOn(ctx, wd)
		}
		return translateErr(turnOff(ctx, sess))

	case cmd.GetVolumeAbsolute() != nil:
		return translateErr(sess.SetVolumeAbsolute(ctx, cmd.GetVolumeAbsolute().GetLevel()))

	case cmd.GetVolumeRelative() != nil:
		st := sess.Status()
		target := st.Volume + cmd.GetVolumeRelative().GetDelta()
		if target < 0 {
			target = 0
		} else if target > 100 {
			target = 100
		}
		return translateErr(sess.SetVolumeAbsolute(ctx, target))

	case cmd.GetMute() != nil:
		return translateErr(sess.SetMuted(ctx, cmd.GetMute().GetIsMuted()))

	case cmd.GetAppLaunch() != nil:
		return translateErr(sess.LaunchApp(ctx, cmd.GetAppLaunch().GetApplicationId()))

	case cmd.GetAppClose() != nil:
		st := sess.Status()
		return translateErr(sess.CloseApp(ctx, st.ForegroundID))

	case cmd.GetChannelAbsolute() != nil:
		return translateErr(sess.SetChannel(ctx, cmd.GetChannelAbsolute().GetChannel()))

	case cmd.GetChannelRelative() != nil:
		return translateErr(sess.ChannelStep(ctx, cmd.GetChannelRelative().GetDelta()))

	default:
		return status.Error(codes.InvalidArgument, "unrecognized command")
	}
}

// turnOn requests the screen turn on, per the handoff's SetPower(true)
// design: if the device already has a live connection (active-standby or
// already on), TurnOnScreen alone is sufficient. If it's fully unreachable
// (socket closed - full standby, or powered off entirely), a Wake-on-LAN
// packet is sent first and this waits for the device to become reachable
// before issuing TurnOnScreen.
func turnOn(ctx context.Context, wd *webosDevice) error {
	sess := wd.session

	if sess.Status().Reachable {
		return translateErr(sess.TurnOnScreen(ctx))
	}

	if wd.cfg.MAC == "" {
		return status.Error(codes.FailedPrecondition, "device unreachable and no mac address configured for wake-on-lan")
	}
	if err := webosctrl.SendMagicPacket(wd.cfg.MAC); err != nil {
		return status.Errorf(codes.Internal, "sending wake-on-lan packet: %v", err)
	}

	wakeCtx, cancel := context.WithTimeout(ctx, wakeTimeout)
	defer cancel()

	ticker := time.NewTicker(wakePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if sess.Status().Reachable {
				return translateErr(sess.TurnOnScreen(ctx))
			}
		case <-wakeCtx.Done():
			return status.Error(codes.DeadlineExceeded, "device did not become reachable after wake-on-lan")
		}
	}
}

// turnOff requests the screen turn off. Not every firmware build exposes
// com.webos.service.tvpower's turnOffScreen method (confirmed 404 on a real
// webOS 5.5 TV) - when that happens this falls back to a full system
// standby instead, which is universally supported but closes the socket
// (losing the active-standby state SetOnOff(true) prefers to resume from).
func turnOff(ctx context.Context, sess webosSession) error {
	err := sess.TurnOffScreen(ctx)
	if err == nil || !errors.Is(err, webosctrl.ErrRequestRejected) {
		return err
	}
	return sess.PowerOff(ctx)
}

// translateErr maps webosctrl's sentinel errors onto gRPC status codes.
// Anything else is wrapped as Internal.
//
// Uses errors.Is rather than a switch on err's exact value: session.go's
// request()/register() always return ErrRequestRejected wrapped via
// fmt.Errorf("%w: %s", ...) to carry the device's error text, which a plain
// switch/== comparison never matches - every real device rejection was
// silently falling through to the generic Internal case instead of
// FailedPrecondition.
func translateErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, webosctrl.ErrPairingRequired):
		return status.Error(codes.FailedPrecondition, "device is not paired yet - accept the on-screen prompt on the TV")
	case errors.Is(err, webosctrl.ErrRequestRejected):
		return status.Error(codes.FailedPrecondition, "device rejected the request")
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
