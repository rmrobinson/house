package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/bridges/frigate/frigate"
	"github.com/rmrobinson/house/service/bridge"
)

const (
	cameraRestreamFormat = "rtsp://%s:8554/%s"
)

// CameraConfig includes basic configuration data for a specific camera identified using its Name
type CameraConfig struct {
	Name         string
	Manufacturer string
	ModelID      string
}

// FrigateBridge is a bridge between the Frigate NVR system and the house.
type FrigateBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	client                 *frigate.Client
	cameraRestreamHostname string

	cameras map[string]*Camera
}

// NewFrigateBridge returns a new instance of the Frigate bridge.
func NewFrigateBridge(logger *zap.Logger, svc *bridge.Service, client *frigate.Client, cameraRestreamHostname string) *FrigateBridge {
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
		logger:                 logger,
		svc:                    svc,
		b:                      b,
		client:                 client,
		cameraRestreamHostname: cameraRestreamHostname,
		cameras:                map[string]*Camera{},
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

// Setup loads the configured cameras into the bridge for use. It then retrieves initial state and errors if it can't reach the Frigate API.
func (fb *FrigateBridge) Setup(ctx context.Context, cameras []CameraConfig) error {
	for _, camera := range cameras {
		fb.cameras[camera.Name] = fb.newCamera(camera)
	}

	config, err := fb.client.GetConfig(ctx)
	if err != nil {
		fb.logger.Error("unable to get config from frigate",
			zap.Error(err))
		return status.Error(codes.Internal, "unable to get config from frigate")
	}
	stats, err := fb.client.GetStats(ctx)
	if err != nil {
		fb.logger.Error("unable to get stats from frigate",
			zap.Error(err))
		return status.Error(codes.Internal, "unable to get stats from frigate")
	}

	for cameraName, frigateCameraConfig := range config.Cameras {
		ep, err := url.Parse(fmt.Sprintf(cameraRestreamFormat, fb.cameraRestreamHostname, cameraName))
		if err != nil {
			fb.logger.Error("unable to parse camera restream endpoint as url", zap.Error(err))
			return err
		}

		if camera, cameraPresent := fb.cameras[cameraName]; cameraPresent {
			camera.Enabled = frigateCameraConfig.Enabled
			camera.Endpoint = ep

			if cameraStats, statsPresent := stats.Cameras[cameraName]; statsPresent {
				camera.Active = (cameraStats.CameraFPS > 0)
				camera.LastActivity = time.Now() // TODO: use the 'events' feed for this
			}

			fb.cameras[cameraName] = camera
			fb.svc.UpdateDevice(camera.ToDevice())
		} else {
			// In this case we haven't gotten an initial config for this camera but we can mark the Model and Manufacturer as unknown
			camera := fb.newCamera(CameraConfig{Name: frigateCameraConfig.Name, Manufacturer: "Unknown", ModelID: "Unknown"})
			camera.Enabled = frigateCameraConfig.Enabled
			camera.Endpoint = ep

			if cameraStats, statsPresent := stats.Cameras[cameraName]; statsPresent {
				camera.Active = (cameraStats.CameraFPS > 0)
				camera.LastActivity = time.Now() // TODO: use the 'events' feed for this
			}

			fb.cameras[cameraName] = camera
			fb.svc.UpdateDevice(camera.ToDevice())
		}
	}

	return nil
}

func (fb *FrigateBridge) newCamera(config CameraConfig) *Camera {
	idBytes := sha256.Sum256([]byte(fmt.Sprintf("%s:%s", fb.client.GetIP(), config.Name)))

	return &Camera{
		ID:           hex.EncodeToString(idBytes[:]),
		Name:         config.Name,
		Manufacturer: config.Manufacturer,
		ModelID:      config.ModelID,
		Enabled:      false,
		Active:       false,
	}
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it queries
// the Frigate API and returns the current state of the cameras.
func (fb *FrigateBridge) Refresh(ctx context.Context) error {
	stats, err := fb.client.GetStats(ctx)
	if err != nil {
		fb.logger.Error("unable to get stats from frigate",
			zap.Error(err))
		return status.Error(codes.Internal, "unable to get stats from frigate")
	}

	for cameraName, camera := range fb.cameras {
		if cameraStats, statsPresent := stats.Cameras[cameraName]; statsPresent {
			camera.Active = (cameraStats.CameraFPS > 0)
			camera.LastActivity = time.Now() // TODO: use the 'events' feed for this
			fb.cameras[cameraName] = camera
			fb.svc.UpdateDevice(camera.ToDevice())
		}
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
