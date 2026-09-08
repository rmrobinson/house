package bridge

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// These are errors bridge implementations may return.
var (
	// ErrUnsupportedCommand is returned if the device can't process the supplied command.
	ErrUnsupportedCommand = status.Error(codes.FailedPrecondition, "device does not support specified command")
	// ErrInvalidTimezone is returned if a specified timezone string isn't valid on the device.
	ErrInvalidTimezone = status.Error(codes.InvalidArgument, "invalid timezone specified")
	// ErrAsyncCommandsNotSupported is returned by Handler.ProcessCommandAsync implementations
	// that have no async command path. In practice ExecuteCommandAsync's device/command
	// type-compatibility check rejects these before the handler is ever called, so bridges
	// with no async-eligible device traits can return this unconditionally.
	ErrAsyncCommandsNotSupported = status.Error(codes.Unimplemented, "bridge does not support asynchronous commands")
)
