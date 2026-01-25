package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/bridges/frigate/frigate"
	"github.com/rmrobinson/house/service/bridge"
)

func cameraToDevice(id string, c frigate.Camera) *device.Device {
	return &device.Device{
		Id:           id,
		ModelId:      "",
		Manufacturer: "",
		LastSeen:     timestamppb.Now(),
		Details: &device.Device_Camera{
			Camera: &device.Camera{
				MediaStream: &trait.MediaStream{
					State: &trait.MediaStream_State{
						Url: c.Endpoint.String(),
					},
				},
				Presence: &trait.Presence{
					State: &trait.Presence_State{},
				},
			},
		},
	}
}

// FrigateBridge is a bridge between the Frigate NVR system and the house.
type FrigateBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	client *frigate.Client
}

// NewFrigateBridge returns a new instance of the Frigate bridge.
func NewFrigateBridge(logger *zap.Logger, svc *bridge.Service, client *frigate.Client) *FrigateBridge {
	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "Frigate11",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
			Address: &api2.Address{
				Ip: &api2.Address_Ip{
					Host: client.GetIP(),
					Port: int32(client.GetPort()),
				},
			},
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}
	return &FrigateBridge{
		logger: logger,
		svc:    svc,
		b:      b,
		client: client,
	}
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (fb *FrigateBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	fb.logger.Error("received unsupported command - shouldn't happen")
	return nil, bridge.ErrUnsupportedCommand
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (fb *FrigateBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	fb.b.Config.Name = config.Name
	fb.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it queries
// the Frigate API and returns the current state of the cameras.
func (fb *FrigateBridge) Refresh(ctx context.Context) error {
	cameras, err := fb.client.GetCameras(ctx)
	if err != nil {
		fb.logger.Error("unable to get cameras from frigate",
			zap.Error(err))
		return status.Error(codes.Internal, "unable to get cameras from frigate")
	}

	for _, camera := range cameras {
		idBytes := sha256.Sum256([]byte(fmt.Sprintf("%s:%s", fb.client.GetIP(), camera.Name)))
		fb.svc.UpdateDevice(cameraToDevice(hex.EncodeToString(idBytes[:]), camera))
	}
	return nil
}

// Run begins the process of polling the sensor and reporting back the state.
func (fb *FrigateBridge) Run(ctx context.Context) {
	err := fb.Refresh(ctx)
	if err != nil {
		fb.logger.Error("unable to refresh bridges", zap.Error(err))
		return
	}

	refreshTimer := time.NewTicker(time.Minute * 5)
	for {
		select {
		case <-refreshTimer.C:
			err := fb.Refresh(ctx)
			if err != nil {
				fb.logger.Error("unable to get cameras from frigate",
					zap.Error(err))
			} else {
				fb.logger.Debug("refreshed")
			}
		}
	}
}
