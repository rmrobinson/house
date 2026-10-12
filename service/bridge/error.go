package bridge

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// These are errors bridge implementations may return.
var (
	// ErrUnsupportedCommand is returned if the device can't process the supplied command.
	ErrUnsupportedCommand = status.Error(codes.FailedPrecondition, "device does not support specified command")
	// ErrArgumentNotSupportedByDevice is returned when a command has values outside the set the device's attributes allow.
	ErrArgumentNotSupportedByDevice = status.Error(codes.InvalidArgument, "the supplied arguments aren't supported by the device attributes")
	// ErrInvalidTimezone is returned if a specified timezone string isn't valid on the device.
	ErrInvalidTimezone = status.Error(codes.InvalidArgument, "invalid timezone specified")
	// ErrAsyncCommandsNotSupported is returned by Handler.ProcessCommandAsync implementations
	// that have no async command path. In practice ExecuteCommandAsync's device/command
	// type-compatibility check rejects these before the handler is ever called, so bridges
	// with no async-eligible device traits can return this unconditionally.
	ErrAsyncCommandsNotSupported = status.Error(codes.Unimplemented, "bridge does not support asynchronous commands")
	// ErrCommandTimeout is returned when a bridge writes a command to a device but doesn't
	// observe the device confirm it within the bridge's own timeout.
	ErrCommandTimeout = status.Error(codes.DeadlineExceeded, "device did not confirm command in time")
	// ErrCommandSuperseded is returned for a command that a newer command to the same device
	// replaced before (or while) it was sent - see CommandLane. The device may still apply it
	// (a command already on the wire can't be recalled), but the caller should treat the newer
	// command as the one that counts.
	ErrCommandSuperseded = status.Error(codes.Aborted, "command superseded by a newer command to the same device")
	// ErrCommandStale is returned for a command that waited too long for its turn at a busy
	// device and was dropped rather than delivered late - see CommandLane.
	ErrCommandStale = status.Error(codes.DeadlineExceeded, "command expired while waiting for the device to finish an earlier one")
	// ErrHistoryNotSupported is returned by GetDeviceHistory when the bridge's handler
	// doesn't implement HistoryHandler.
	ErrHistoryNotSupported = status.Error(codes.Unimplemented, "bridge does not support device history")
	// ErrStreamFellBehind ends a StreamUpdates stream whose client stopped keeping up with
	// updates (see Source.SendMessage). The client should reconnect to resync from a fresh
	// initial snapshot.
	ErrStreamFellBehind = status.Error(codes.Unavailable, "update stream fell behind; reconnect to resync")
)
