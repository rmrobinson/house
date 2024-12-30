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
	// ErrArgumentNotSupportedByDevice is returned when a command has values outside the specified supported set by the device attributes.
	ErrArgumentNotSupportedByDevice = status.Error(codes.InvalidArgument, "the supplied arguments aren't supported by the device attributes")
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

	if d.GetAvReceiver() != nil {
		if req.GetOnOff() != nil {
			if !d.GetAvReceiver().GetOnOff().Attributes.CanControl {
				logger.Info("received onoff request to a device which can't be controlled")
				return nil, ErrArgumentNotSupportedByDevice
			}
			logger.Debug("processing onoff command")
			return a.svc.processCommand(ctx, req)
		} else if req.GetVolume() != nil {
			if !d.GetAvReceiver().GetVolume().Attributes.CanControl {
				logger.Info("received volume request to a device which can't be controlled")
				return nil, ErrArgumentNotSupportedByDevice
			} else if req.GetVolume().Muted != nil && !d.GetAvReceiver().GetVolume().GetAttributes().CanMute {
				logger.Info("received mute request to device which can't be muted")
				return nil, ErrArgumentNotSupportedByDevice
			} else if req.GetVolume().Volume < 0 || req.GetVolume().Volume > d.GetAvReceiver().GetVolume().Attributes.MaximumLevel {
				logger.Info("received volume request with a volume outside the supported range",
					zap.Int32("requested_volume", req.GetVolume().Volume),
					zap.Int32("volume_limit", d.GetAvReceiver().Volume.Attributes.MaximumLevel))
				return nil, ErrArgumentNotSupportedByDevice
			}
			logger.Debug("processing volume command")
			return a.svc.processCommand(ctx, req)
		} else if req.GetInput() != nil {
			if !d.GetAvReceiver().GetInput().Attributes.CanControl {
				logger.Info("received input request to a device which can't be controlled")
				return nil, ErrArgumentNotSupportedByDevice
			}
			validInputID := false
			for _, input := range d.GetAvReceiver().GetInput().GetAttributes().Inputs {
				if req.GetInput().InputId == input.Id {
					validInputID = true
					break
				}
			}
			if !validInputID {
				logger.Info("received input request with an unknown input id",
					zap.String("received_input_id", req.GetInput().InputId))
				return nil, ErrArgumentNotSupportedByDevice
			}
			logger.Debug("processing input command")
			return a.svc.processCommand(ctx, req)
		} else if req.GetAudioOutput() != nil {
			if !d.GetAvReceiver().GetAudioOutput().Attributes.CanControl {
				logger.Info("received audio output request to a device which can't be controlled")
				return nil, ErrArgumentNotSupportedByDevice
			}
			if req.GetAudioOutput().Balance != nil && d.GetAvReceiver().GetAudioOutput().GetState().Balance == nil {
				logger.Info("received audio output request to change balance on a device without balance set")
				return nil, ErrArgumentNotSupportedByDevice
			}
			logger.Debug("processing audio output command")
			return a.svc.processCommand(ctx, req)
		}
	} else if d.GetClock() != nil {
		if req.GetOnOff() != nil {
			if !d.GetClock().GetOnOff().Attributes.CanControl {
				logger.Info("received onoff request to a device which can't be controlled")
				return nil, ErrArgumentNotSupportedByDevice
			}
			logger.Debug("processing onoff command")
			return a.svc.processCommand(ctx, req)
		} else if d.GetClock().GetBrightness() != nil && (req.GetBrightnessRelative() != nil || req.GetBrightnessAbsolute() != nil) {
			if !d.GetClock().GetBrightness().Attributes.CanControl {
				logger.Info("received brightness request to a device which can't be controlled")
				return nil, ErrArgumentNotSupportedByDevice
			}
			logger.Debug("processing brightness command")
			return a.svc.processCommand(ctx, req)
		} else if d.GetClock().GetTime() != nil && req.GetTime() != nil {
			if !d.GetClock().GetTime().Attributes.CanControl {
				logger.Info("received time request to a device which can't be controlled")
				return nil, ErrArgumentNotSupportedByDevice
			}
			logger.Debug("processing time command")
			return a.svc.processCommand(ctx, req)
		}
	} else if d.GetLight() != nil {
		if req.GetOnOff() != nil {
			if !d.GetLight().GetOnOff().Attributes.CanControl {
				logger.Info("received onoff request to a device which can't be controlled")
				return nil, ErrArgumentNotSupportedByDevice
			}
			logger.Debug("processing onoff command")
			return a.svc.processCommand(ctx, req)
		} else if d.GetLight().GetBrightness() != nil && (req.GetBrightnessRelative() != nil || req.GetBrightnessAbsolute() != nil) {
			if !d.GetLight().GetBrightness().Attributes.CanControl {
				logger.Info("received brightness request to a device which can't be controlled")
				return nil, ErrArgumentNotSupportedByDevice
			}
			logger.Debug("processing brightness command")
			return a.svc.processCommand(ctx, req)
		}
	} else if d.GetSensor() != nil {
	} else if d.GetThermostat() != nil {
		if req.GetOnOff() != nil {
			if !d.GetThermostat().GetOnOff().Attributes.CanControl {
				logger.Info("received onoff request to a device which can't be controlled")
				return nil, ErrArgumentNotSupportedByDevice
			}
			logger.Debug("processing onoff command")
			return a.svc.processCommand(ctx, req)
		}
	} else if d.GetUps() != nil {
	}

	logger.Info("unsupported command received")
	return nil, ErrCommandNotSupported
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
