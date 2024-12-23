package main

import (
	"context"
	"net/url"
	"strconv"

	"go.uber.org/zap"

	"github.com/spf13/viper"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

// PlexBridge is a bridge to the Plex media server.
type PlexBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	client *Plex
}

// NewChargerBridge creates a new charger bridge
func NewPlexBridge(logger *zap.Logger, svc *bridge.Service, client *Plex) *PlexBridge {
	var host string
	var port int

	serverURL, err := url.Parse(client.serverURL)
	if err != nil {
		host = client.serverURL
	} else {
		host = serverURL.Hostname()
	}
	if len(serverURL.Port()) > 0 {
		port, err = strconv.Atoi(serverURL.Port())
		if err != nil {
			port = 0
		}
	}

	b := &api2.Bridge{
		Id:           client.id,
		IsReachable:  true,
		ModelId:      "PLX1",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        client.name,
			Description: viper.GetString("bridge.description"),
			Address: &api2.Address{
				Ip: &api2.Address_Ip{
					Host: host,
					Port: int32(port),
				},
			},
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	cb := &PlexBridge{
		logger: logger,
		svc:    svc,
		client: client,
		b:      b,
	}

	return cb
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (pb *PlexBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	pb.logger.Error("received unsupported command - shouldn't happen")
	return nil, bridge.ErrUnsupportedCommand
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (pb *PlexBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	pb.b.Config.Description = config.Description

	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation
// the Plex API is refreshed.
func (pb *PlexBridge) Refresh(ctx context.Context) error {
	return pb.client.Refresh(ctx)
}
