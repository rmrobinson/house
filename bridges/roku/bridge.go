package main

import (
	"context"
	"strings"
	"time"

	"github.com/picatz/roku"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

func rokuStateToDevice(info *roku.DeviceInfo, apps roku.Apps, activeApp *roku.App) *device.Device {
	inputTrait := &trait.Input{
		Attributes: &trait.Input_Attributes{
			CanControl: true,
			Inputs:     []*trait.Input_InputDetails{},
			IsOrdered:  true,
		},
		State: &trait.Input_State{},
	}
	appTrait := &trait.App{
		Attributes: &trait.App_Attributes{
			CanControl:   true,
			Applications: []*trait.App_Instance{},
		},
		State: &trait.App_State{},
	}

	for _, app := range apps {
		if strings.HasPrefix(app.Name, roku.TVInput) {
			inputTrait.Attributes.Inputs = append(inputTrait.Attributes.Inputs, &trait.Input_InputDetails{
				Id: app.ID, Name: app.Name,
			})
		} else {
			appTrait.Attributes.Applications = append(appTrait.Attributes.Applications, &trait.App_Instance{
				Id:      app.ID,
				Name:    app.Name,
				Version: app.Version,
				Type:    app.Type,
			})
		}
	}
	if activeApp != nil {
		appTrait.State.ApplicationId = activeApp.ID
	}

	var modelName *string
	if len(info.FriendlyModelName) > 0 {
		modelName = &info.FriendlyModelName
	} else {
		modelName = &info.ModelName
	}

	return &device.Device{
		Id:           info.DeviceID,
		ModelId:      info.ModelNumber,
		ModelName:    modelName,
		Manufacturer: info.VendorName,
		Address: &device.Device_Address{
			Address:     info.Udn,
			IsReachable: true,
			HopCount:    1,
		},
		Config: &device.Device_Config{
			Name: info.UserDeviceName,
		},
		LastSeen: timestamppb.Now(),
		Details: &device.Device_Television{
			Television: &device.Television{
				OnOff:  nil, // We don't know the on/off state of the TV
				Volume: nil, // We don't know the volume of the TV
				Input:  inputTrait,
				App:    appTrait,
				Media:  nil, // TODO: determine media info, if available
			},
		},
	}
}

// RokuBridge monitors the network for Roku advertisements and uses the ECP API to perform basic operations.
// There only needs to be one bridge per network, as it will listen for the advertisements and forward all devices.
type RokuBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	endpoints map[string]*roku.Endpoint
}

// NewRokuBridge creates a new Roku bridge
func NewRokuBridge(logger *zap.Logger, svc *bridge.Service) *RokuBridge {
	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "RB1",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
			Address: &api2.Address{
				Ip: &api2.Address_Ip{},
			},
		},
	}

	return &RokuBridge{
		logger:    logger,
		svc:       svc,
		b:         b,
		endpoints: map[string]*roku.Endpoint{},
	}
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (rb *RokuBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	rb.logger.Error("received unsupported command - shouldn't happen")
	return nil, bridge.ErrUnsupportedCommand
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (rb *RokuBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	rb.b.Config.Description = config.Description

	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it
// finds all the available devices and reports them.
func (rb *RokuBridge) Refresh(ctx context.Context) error {
	endpoints, err := roku.Find(roku.DefaultWaitTime)
	if err != nil {
		rb.logger.Error("unable to refresh roku endpoints",
			zap.Error(err))
		return status.Error(codes.Internal, "unable to refresh roku endpoints")
	}

	foundEndpoints := map[string]bool{}

	for _, endpoint := range endpoints {
		info, err := endpoint.DeviceInfo()
		if err != nil {
			rb.logger.Error("unable to get roku device info",
				zap.Error(err), zap.String("endpoint", endpoint.String()))
			continue
		}
		apps, err := endpoint.Apps()
		if err != nil {
			rb.logger.Error("unable to get roku apps",
				zap.Error(err), zap.String("endpoint", endpoint.String()))
			continue
		}
		activeApp, err := endpoint.ActiveApp()
		if err != nil {
			rb.logger.Error("unable to get roku active app",
				zap.Error(err), zap.String("endpoint", endpoint.String()))
			continue
		}

		rb.svc.UpdateDevice(rokuStateToDevice(info, apps, activeApp))
		foundEndpoints[info.DeviceID] = true
		rb.endpoints[info.DeviceID] = endpoint
	}

	for existingEndpointID := range rb.endpoints {
		if _, found := foundEndpoints[existingEndpointID]; !found {
			rb.logger.Info("device id not found in set of endpoints, removing", zap.String("device_id", existingEndpointID))
			rb.svc.RemoveDevice(existingEndpointID)
		}
	}

	return nil
}

// Run begins the process of rerunning the Roku discovery process on an interval and updating the device cache.
func (rb *RokuBridge) Run(ctx context.Context) {
	refreshTimer := time.NewTicker(time.Minute * 5)
	for {
		select {
		case <-refreshTimer.C:
			if err := rb.Refresh(ctx); err != nil {
				rb.logger.Error("unable to refresh roku state",
					zap.Error(err))
				continue
			}
		case <-ctx.Done():
			rb.logger.Info("run context cancelled")
			return
		}
	}
}
