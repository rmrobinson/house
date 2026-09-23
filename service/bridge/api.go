package bridge

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
)

// These errors are ones that the API itself can return
var (
	// ErrBridgeNotReady is returned if the bridge is in the process of initializing and isn't ready to process requests
	ErrBridgeNotReady = status.Error(codes.Unavailable, "bridge not ready")
	// ErrDeviceNotFound is returned when a specified device ID is requested but isn't registered.
	ErrDeviceNotFound = status.Error(codes.NotFound, "device id not found")
	// ErrCommandNotSupported is returned when a command is targeted to a device which doesn't support the specified command type.
	ErrCommandNotSupported = status.Error(codes.InvalidArgument, "the device does not support the specified command")
	// ErrVersionMismatch is returned when Command.version is set and doesn't match the
	// device's current Device.version.
	ErrVersionMismatch = status.Error(codes.FailedPrecondition, "device has changed since the command's version was read")
	// ErrCommandIDRequired is returned by ExecuteCommandAsync when Command.id
	// is empty. It's the only way a caller can correlate the RPC it made with
	// the CommandUpdate reporting its outcome, so unlike ExecuteCommand (which
	// has no such correlation problem, since its RPC response is the outcome)
	// it can't be left optional there.
	ErrCommandIDRequired = status.Error(codes.InvalidArgument, "command id is required for asynchronous execution")
)

// API contains the required implementation to confirm to the gRPC api.Bridge interface.
// It exists as a separate struct from the Service struct to ensure there is not confusion
// for those who want to utilize the Service type in their code.
// This simply wraps the incoming API requests and performs the required buffering for the streaming calls.
type API struct {
	logger *zap.Logger

	svc *Service
}

func newAPI(logger *zap.Logger, svc *Service) *API {
	return &API{
		logger: logger,
		svc:    svc,
	}
}

func (a *API) GetBridge(ctx context.Context, req *api2.GetBridgeRequest) (*api2.Bridge, error) {
	if a.svc.bridge == nil {
		return nil, ErrBridgeNotReady
	}
	return a.svc.getBridge(), nil
}

func (a *API) ListDevices(ctx context.Context, req *api2.ListDevicesRequest) (*api2.ListDevicesResponse, error) {
	if a.svc.bridge == nil {
		return nil, ErrBridgeNotReady
	}

	ret := &api2.ListDevicesResponse{
		Devices: a.svc.getDevices(),
	}
	return ret, nil
}

func (a *API) GetDevice(ctx context.Context, req *api2.GetDeviceRequest) (*device.Device, error) {
	if a.svc.bridge == nil {
		return nil, ErrBridgeNotReady
	}

	d := a.svc.getDevice(req.GetId())
	if d != nil {
		return d, nil
	}
	return nil, ErrDeviceNotFound
}

func (a *API) UpdateDeviceConfig(ctx context.Context, req *api2.UpdateDeviceConfigRequest) (*device.Device, error) {
	return nil, status.Errorf(codes.Unimplemented, "not available")
}

// deviceSupportsCommand reports whether the given device's populated traits make it
// eligible for the given command's type. Shared between ExecuteCommand and
// ExecuteCommandAsync so the device-type/command-type compatibility rules only live
// in one place.
func deviceSupportsCommand(d *device.Device, req *command.Command) bool {
	if d.GetGeneric() != nil {
		g := d.GetGeneric()
		if g.GetOnOff() != nil && req.GetOnOff() != nil {
			return true
		} else if g.GetBrightness() != nil && (req.GetBrightnessRelative() != nil || req.GetBrightnessAbsolute() != nil) {
			return true
		} else if g.GetColour() != nil && req.GetColour() != nil {
			return true
		}
	} else if d.GetAvReceiver() != nil {
		return req.GetOnOff() != nil
	} else if d.GetClock() != nil {
		if req.GetOnOff() != nil {
			return true
		} else if d.GetClock().GetBrightness() != nil && (req.GetBrightnessRelative() != nil || req.GetBrightnessAbsolute() != nil) {
			return true
		} else if d.GetClock().GetTime() != nil && req.GetTime() != nil {
			return true
		}
	} else if d.GetLight() != nil {
		if req.GetOnOff() != nil {
			return true
		} else if d.GetLight().GetBrightness() != nil && (req.GetBrightnessRelative() != nil || req.GetBrightnessAbsolute() != nil) {
			return true
		} else if d.GetLight().GetColour() != nil && req.GetColour() != nil {
			return true
		} else if d.GetLight().GetScene() != nil && req.GetAppLaunch() != nil {
			return true
		}
	} else if d.GetThermostat() != nil {
		return req.GetOnOff() != nil
	} else if d.GetFan() != nil {
		// Fan had no branch here at all until bridges/zigbee's fanBuilder became the first
		// device.Fan producer in this repo and every command against it was rejected here before
		// ever reaching a bridge - found via live testing against a real IKEA STARKVIND air
		// purifier. Mode/Toggles are only eligible when the specific device actually reports that
		// trait, mirroring every other branch here; OnOff is unconditional since Fan.OnOff (like
		// Light.OnOff) is always present. No Speed branch: every Fan.Speed this repo's one
		// producer (fanBuilder) ever builds is read-only telemetry (Attributes.can_control always
		// false, confirmed against real hardware whose only speed control is its Mode enum) - add
		// one only once some builder actually implements a Speed command against a Fan.
		if req.GetOnOff() != nil {
			return true
		} else if d.GetFan().GetMode() != nil && req.GetMode() != nil {
			return true
		} else if d.GetFan().GetToggles() != nil && req.GetToggle() != nil {
			return true
		}
	} else if d.GetMediaPlayer() != nil {
		if d.GetMediaPlayer().GetVolume() != nil && (req.GetVolumeAbsolute() != nil || req.GetVolumeRelative() != nil || req.GetMute() != nil) {
			return true
		} else if d.GetMediaPlayer().GetMedia() != nil && (req.GetPlayback() != nil || req.GetSeekAbsolute() != nil || req.GetSeekRelative() != nil) {
			return true
		} else if d.GetMediaPlayer().GetApp() != nil && req.GetAppLaunch() != nil {
			return true
		}
	} else if d.GetTelevision() != nil {
		// Volume/Mute/Playback/Seek/AppLaunch against a real Cast-driven
		// Television device (Google TV Streamer) are confirmed live: writes
		// reach the device and are acknowledged. On hardware reporting
		// Volume.controlType "fixed" (the TV/soundbar owns volume via HDMI-CEC,
		// not the Cast receiver), Volume/Mute round-trip successfully at the
		// protocol level but the device itself declines to act on them,
		// surfacing an on-screen prompt to use the physical remote instead —
		// see bridges/cast/README.md. That's a hardware-ownership fact about
		// this controlType, not a gap in this dispatch path.
		if d.GetTelevision().GetOnOff() != nil && req.GetOnOff() != nil {
			return true
		} else if d.GetTelevision().GetVolume() != nil && (req.GetVolumeAbsolute() != nil || req.GetVolumeRelative() != nil || req.GetMute() != nil) {
			return true
		} else if d.GetTelevision().GetMedia() != nil && (req.GetPlayback() != nil || req.GetSeekAbsolute() != nil || req.GetSeekRelative() != nil) {
			return true
		} else if d.GetTelevision().GetApp() != nil && (req.GetAppLaunch() != nil || req.GetAppClose() != nil) {
			return true
		} else if d.GetTelevision().GetChannel() != nil && (req.GetChannelAbsolute() != nil || req.GetChannelRelative() != nil) {
			return true
		}
	}

	return false
}

// checkVersion enforces Device.version's optimistic-concurrency contract
// (api/device/device.proto): if req.Version is set, it must match d's
// current version, or the command is rejected before anything is sent to
// the device. An empty req.Version means execute unconditionally. Shared
// between ExecuteCommand and ExecuteCommandAsync so individual bridges don't
// need to reimplement this check themselves.
func checkVersion(d *device.Device, req *command.Command) error {
	if req.GetVersion() != "" && req.GetVersion() != d.GetVersion() {
		return ErrVersionMismatch
	}
	return nil
}

func (a *API) ExecuteCommand(ctx context.Context, req *command.Command) (*device.Device, error) {
	if a.svc.bridge == nil {
		return nil, ErrBridgeNotReady
	}

	logger := a.logger.With(zap.String("device_id", req.DeviceId))
	d := a.svc.getDevice(req.DeviceId)
	if d == nil {
		logger.Debug("request made for unknown device")
		return nil, ErrDeviceNotFound
	}

	if !deviceSupportsCommand(d, req) {
		logger.Info("unsupported command received")
		return nil, ErrCommandNotSupported
	}

	if err := checkVersion(d, req); err != nil {
		logger.Info("stale command version received", zap.String("command_version", req.GetVersion()), zap.String("device_version", d.GetVersion()))
		return nil, err
	}

	logger.Debug("processing command")
	return a.svc.processCommand(ctx, req)
}

// ExecuteCommandAsync dispatches a command without blocking for the device to confirm,
// per bridge.proto's documented contract: every command accepted here (device found,
// command type supported) produces exactly one terminal CommandUpdate on the update
// stream, and the RPC response itself carries no result. A caller must already be
// streaming via StreamUpdates to observe the outcome.
func (a *API) ExecuteCommandAsync(ctx context.Context, req *command.Command) (*api2.ExecuteCommandAsyncResponse, error) {
	if a.svc.bridge == nil {
		return nil, ErrBridgeNotReady
	}

	logger := a.logger.With(zap.String("device_id", req.DeviceId), zap.String("command_id", req.Id))
	d := a.svc.getDevice(req.DeviceId)
	if d == nil {
		logger.Debug("request made for unknown device")
		return nil, ErrDeviceNotFound
	}

	if !deviceSupportsCommand(d, req) {
		logger.Info("unsupported command received")
		return nil, ErrCommandNotSupported
	}

	// Checked only once the command would otherwise be accepted (device
	// found, command type supported) — Id is what lets the caller correlate
	// this RPC with the CommandUpdate reporting its outcome, so an empty one
	// makes the command impossible to track and must be rejected synchronously
	// rather than accepted and silently uncorrelatable.
	if req.GetId() == "" {
		logger.Info("async command received with no id")
		return nil, ErrCommandIDRequired
	}

	// Unlike ExecuteCommand, a stale version here is not a synchronous RPC
	// error — per bridge.proto's contract every *accepted* command (device
	// found, command type supported) produces exactly one terminal
	// CommandUpdate on the stream, version conflict included, so the caller
	// only needs to be watching one place for the outcome.
	if err := checkVersion(d, req); err != nil {
		logger.Info("stale command version received", zap.String("command_version", req.GetVersion()), zap.String("device_version", d.GetVersion()))
		a.svc.CompleteCommand(req, err, nil)
		return &api2.ExecuteCommandAsyncResponse{}, nil
	}

	logger.Debug("dispatching command asynchronously")
	if err := a.svc.handler.ProcessCommandAsync(ctx, req); err != nil {
		a.svc.CompleteCommand(req, err, nil)
	}

	return &api2.ExecuteCommandAsyncResponse{}, nil
}

func (a *API) StreamUpdates(req *api2.StreamUpdatesRequest, stream api2.BridgeService_StreamUpdatesServer) error {
	peer, ok := peer.FromContext(stream.Context())
	addr := "unknown"
	if ok {
		addr = peer.Addr.String()
	}

	logger := a.logger.With(zap.String("peer_addr", addr))
	logger.Debug("bridge stream initialized")

	initUpdate := &api2.Update{
		Action: api2.Update_INITIAL,
		Update: &api2.Update_InitialUpdate{
			InitialUpdate: &api2.InitialUpdate{
				Bridge:  a.svc.getBridge(),
				Devices: a.svc.getDevices(),
			},
		},
	}
	if err := stream.Send(initUpdate); err != nil {
		logger.Error("failed to send initial state", zap.Error(err))
		return err
	}

	// TODO: there is a race condition here where we snap the state of the devices and then
	// a device update happens before we subscribe to the update stream. This will be fixed
	// in a follow-on PR.

	sink := a.svc.updates.NewSink()
	defer sink.Close()

	for {
		select {
		case <-stream.Context().Done():
			// TODO: log that the channel is closed
			err := stream.Context().Err()
			logger.Info("grpc stream closed", zap.Error(err))
			return nil
		case msg, ok := <-sink.Messages():
			if !ok {
				logger.Info("sink stream closed")
				return nil
			}

			update, castOk := msg.(*api2.Update)
			if !castOk {
				panic("must send api2.Update messages to the updates chan")
			}

			if err := stream.Send(update); err != nil {
				logger.Error("unable to send update", zap.Error(err))
				return err
			}
		}
	}
}
