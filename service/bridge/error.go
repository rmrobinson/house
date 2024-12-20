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
)
