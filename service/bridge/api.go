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
			logger.Debug("processing onoff command")
			return a.svc.processCommand(ctx, req)
		}
	} else if d.GetClock() != nil {
		if req.GetOnOff() != nil {
			logger.Debug("processing onoff command")
			return a.svc.processCommand(ctx, req)
		} else if d.GetClock().GetBrightness() != nil && (req.GetBrightnessRelative() != nil || req.GetBrightnessAbsolute() != nil) {
			logger.Debug("processing brightness command")
			return a.svc.processCommand(ctx, req)
		} else if d.GetClock().GetTime() != nil && req.GetTime() != nil {
			logger.Debug("processing time command")
			return a.svc.processCommand(ctx, req)
		}
	} else if d.GetLight() != nil {
		if req.GetOnOff() != nil {
			logger.Debug("processing onoff command")
			return a.svc.processCommand(ctx, req)
		} else if d.GetLight().GetBrightness() != nil && (req.GetBrightnessRelative() != nil || req.GetBrightnessAbsolute() != nil) {
			logger.Debug("processing brightness command")
			return a.svc.processCommand(ctx, req)
		}
	} else if d.GetSensor() != nil {
	} else if d.GetThermostat() != nil {
		if req.GetOnOff() != nil {
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
		Action: api2.Update_INIT,
		Update: &api2.Update_InitUpdate{
			InitUpdate: &api2.InitUpdate{
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
