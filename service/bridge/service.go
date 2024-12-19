package bridge

import (
	"context"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
)

// Config contains the settable parts of the bridge configuration
type Config struct {
	Name        string
	Description string
}

// Handler defines the methods that a registered bridge needs to be ready to handle.
type Handler interface {
	SetBridgeConfig(ctx context.Context, config Config) error

	ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error)
	Refresh(ctx context.Context) error
}

// Service contains the relevant fields to allow management of devices on the bridge.
type Service struct {
	logger *zap.Logger
	api    *API

	handler Handler

	bridge *api2.Bridge

	devices     map[string]*device.Device
	devicesLock sync.Mutex

	updates *Source
}

// NewService creates a new device service
func NewService(logger *zap.Logger) *Service {
	svc := &Service{
		logger:  logger,
		devices: make(map[string]*device.Device),
		updates: NewSource(logger),
	}
	svc.api = newAPI(logger, svc)

	return svc
}

// API() returns the associated gRPC API for this service implementation.
func (s *Service) API() *API {
	return s.api
}

// RegisterHandler is to be called by the bridge implementation when it is ready to begin processing requests.
func (s *Service) RegisterHandler(h Handler, b *api2.Bridge) {
	if h == nil || b == nil {
		s.logger.Fatal("nil handler or bridge supplied")
	}

	s.handler = h
	s.bridge = b

	s.updates.SendMessage(&api2.Update{
		Action: api2.Update_ADDED,
		Update: &api2.Update_BridgeUpdate{
			BridgeUpdate: &api2.BridgeUpdate{
				BridgeId: s.bridge.GetId(),
				Bridge:   proto.Clone(s.bridge).(*api2.Bridge),
			},
		},
	})
}

// UpdateBridge takes the supplied bridge info and updates it within the service.
// This should be called by bridge implementations when a change to the underlying bridge is detected.
func (s *Service) UpdateBridge(b *api2.Bridge) {
	if b == nil {
		s.logger.Fatal("nil bridge supplied")
	}

	s.bridge = b

	s.updates.SendMessage(&api2.Update{
		Action: api2.Update_CHANGED,
		Update: &api2.Update_BridgeUpdate{
			BridgeUpdate: &api2.BridgeUpdate{
				BridgeId: s.bridge.GetId(),
				Bridge:   proto.Clone(s.bridge).(*api2.Bridge),
			},
		},
	})
}

// UpdateDevice takes the supplied device and updates it within the service.
// If the device doesn't exist it is registered first.
// This should be called by bridge implementations when a change to the underlying device is detected.
func (s *Service) UpdateDevice(d *device.Device) {
	s.devicesLock.Lock()
	defer s.devicesLock.Unlock()

	if d == nil {
		s.logger.Fatal("nil device supplied")
	}

	s.logger.Debug("adding device",
		zap.String("device_id", d.Id),
	)

	_, added := s.devices[d.Id]
	s.devices[d.Id] = d

	action := api2.Update_CHANGED
	if added {
		action = api2.Update_ADDED
	}

	s.updates.SendMessage(&api2.Update{
		Action: action,
		Update: &api2.Update_DeviceUpdate{
			DeviceUpdate: &api2.DeviceUpdate{
				BridgeId: s.bridge.GetId(),
				DeviceId: d.GetId(),
				Device:   proto.Clone(d).(*device.Device),
			},
		},
	})
}

// RemoveDevice removes the specified device ID from the service.
// This should be called by bridge implementations when a removal of the specified device is detected.
func (s *Service) RemoveDevice(id string) {
	s.devicesLock.Lock()
	defer s.devicesLock.Unlock()

	if _, found := s.devices[id]; found {
		delete(s.devices, id)
	}

	s.updates.SendMessage(&api2.Update{
		Action: api2.Update_REMOVED,
		Update: &api2.Update_DeviceUpdate{
			DeviceUpdate: &api2.DeviceUpdate{
				BridgeId: s.bridge.GetId(),
				DeviceId: id,
			},
		},
	})
}

func (s *Service) getBridge() *api2.Bridge {
	return proto.Clone(s.bridge).(*api2.Bridge)
}

func (s *Service) getDevices() []*device.Device {
	s.devicesLock.Lock()
	defer s.devicesLock.Unlock()

	ret := []*device.Device{}
	for _, d := range s.devices {
		ret = append(ret, proto.Clone(d).(*device.Device))
	}

	return ret
}

func (s *Service) getDevice(id string) *device.Device {
	s.devicesLock.Lock()
	defer s.devicesLock.Unlock()

	if d, found := s.devices[id]; found {
		return proto.Clone(d).(*device.Device)
	}
	return nil
}
