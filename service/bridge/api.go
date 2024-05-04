package bridge

import (
	"context"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
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

	deviceUpdates chan *api2.DeviceUpdate
}

func NewAPI(logger *zap.Logger, svc *Service) *API {
	return &API{
		logger:        logger,
		svc:           svc,
		deviceUpdates: make(chan *api2.DeviceUpdate),
	}
}

func (a *API) GetBridge(ctx context.Context, req *api2.GetBridgeRequest) (*api2.Bridge, error) {
	return a.svc.bridge, nil
}

func (a *API) ListDevices(ctx context.Context, req *api2.ListDevicesRequest) (*api2.ListDevicesResponse, error) {
	ret := &api2.ListDevicesResponse{
		Devices: []*device.Device{},
	}
	for _, d := range a.svc.devices {
		ret.Devices = append(ret.Devices, d)
	}

	return ret, nil
}

func (a *API) GetDevice(ctx context.Context, req *api2.GetDeviceRequest) (*device.Device, error) {
	if d, found := a.svc.devices[req.Id]; found {
		return d, nil
	}
	return nil, ErrDeviceNotFound
}

func (a *API) UpdateDeviceConfig(ctx context.Context, req *api2.UpdateDeviceConfigRequest) (*device.Device, error) {
	return nil, status.Errorf(codes.Unimplemented, "not available")
}

func (a *API) ExecuteCommand(ctx context.Context, req *command.Command) (*device.Device, error) {
	logger := a.logger.With(zap.String("device_id", req.DeviceId))
	var d *device.Device
	found := false
	if d, found = a.svc.devices[req.DeviceId]; !found {
		logger.Debug("request made for unknown device")
		return nil, ErrDeviceNotFound
	}

	if d.GetAvReceiver() != nil {
		if req.GetOnOff() != nil {
			logger.Debug("processing onoff command")
			return a.svc.handler.ProcessCommand(ctx, req)
		}
	} else if d.GetClock() != nil {
		if req.GetOnOff() != nil {
			logger.Debug("processing onoff command")
			return a.svc.handler.ProcessCommand(ctx, req)
		} else if d.GetClock().GetBrightness() != nil && (req.GetBrightnessRelative() != nil || req.GetBrightnessAbsolute() != nil) {
			logger.Debug("processing brightness command")
			return a.svc.handler.ProcessCommand(ctx, req)
		} else if d.GetClock().GetTime() != nil && req.GetTime() != nil {
			logger.Debug("processing time command")
			return a.svc.handler.ProcessCommand(ctx, req)
		}
	} else if d.GetLight() != nil {
		if req.GetOnOff() != nil {
			logger.Debug("processing onoff command")
			return a.svc.handler.ProcessCommand(ctx, req)
		} else if d.GetLight().GetBrightness() != nil && (req.GetBrightnessRelative() != nil || req.GetBrightnessAbsolute() != nil) {
			logger.Debug("processing brightness command")
			return a.svc.handler.ProcessCommand(ctx, req)
		}
	} else if d.GetSensor() != nil {
	} else if d.GetThermostat() != nil {
		if req.GetOnOff() != nil {
			logger.Debug("processing onoff command")
			return a.svc.handler.ProcessCommand(ctx, req)
		}
	} else if d.GetUps() != nil {
	}

	logger.Info("unsupported command received")
	return nil, ErrCommandNotSupported
}

func (a *API) StreamUpdates(req *api2.StreamUpdatesRequest, stream api2.BridgeService_StreamUpdatesServer) error {
	// TODO: register this request for updates with the API
	// TODO: stream updates
	return nil
}
