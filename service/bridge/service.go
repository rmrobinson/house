package bridge

import (
	"context"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"go.uber.org/zap"
)

// Service contains the relevant fields to allow management of devices on the bridge.
type Service struct {
	logger *zap.Logger

	handler Handler

	bridge *api2.Bridge

	devices map[string]*device.Device
}

// Handler defines the methods that a registered bridge needs to be ready to handle.
type Handler interface {
	ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error)
	Refresh(ctx context.Context) error
}

// NewService creates a new device service
func NewService(logger *zap.Logger, handler Handler, bridge *api2.Bridge) *Service {
	svc := &Service{
		bridge:  bridge,
		logger:  logger,
		handler: handler,
		devices: make(map[string]*device.Device),
	}

	return svc
}

// UpdateDevice takes the supplied device and updates it within the service.
// If the device doesn't exist it is registered first.
func (s *Service) UpdateDevice(d *device.Device) error {
	if d == nil {
		s.logger.Fatal("nil device supplied")
	}

	s.logger.Debug("adding device",
		zap.String("device_id", d.Id),
	)
	s.devices[d.Id] = d
	return nil
}
