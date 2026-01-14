package main

import (
	"context"
	"strings"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/rmrobinson/omada"
	"github.com/rmrobinson/omada/api"
	omapi "github.com/rmrobinson/omada/api"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

func omClientInfoToDevice(s *omapi.ClientInfo) *device.Device {
	lastSeen := time.Unix(*s.LastSeen, 0)
	cleanMAC := strings.ReplaceAll(*s.Mac, "-", ":")
	cleanMAC = strings.ToUpper(cleanMAC)

	state := &trait.NetworkPresence_State{
		HardwareAddress: cleanMAC,
		IpAddresses:     []string{},
		DeviceCategory:  *s.DeviceType,
	}

	if s.HostName != nil {
		state.Hostname = *s.HostName
	}
	if s.Rssi != nil {
		state.SignalLevel = *s.Rssi
	}
	if s.Ip != nil {
		state.IpAddresses = append(state.IpAddresses, *s.Ip)
	}
	if s.Ipv6List != nil {
		state.IpAddresses = append(state.IpAddresses, *s.Ipv6List...)
	}
	if s.Ssid != nil {
		state.NetworkId = *s.Ssid
	}
	if s.ApName != nil {
		state.NetworkDeviceId = *s.ApName
	}

	return &device.Device{
		Id:       *s.Mac,
		LastSeen: timestamppb.New(lastSeen),
		Details: &device.Device_ConnectedDevice{
			ConnectedDevice: &device.ConnectedDevice{
				NetworkPresence: &trait.NetworkPresence{
					State: state,
				},
			},
		},
	}
}

// OmadaBridge
type OmadaBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	client *omada.Client
	cid    string
	siteID string
}

// NewOmadaBridge creates a new Omada bridge from the supplied client
func NewOmadaBridge(logger *zap.Logger, svc *bridge.Service, client *omada.Client, omadaIPAddr string, omadaPort int, siteID string, cid string) *OmadaBridge {
	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "OM1",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
			Address: &api2.Address{
				Ip: &api2.Address_Ip{
					Host: omadaIPAddr,
					Port: int32(omadaPort),
				},
			},
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	return &OmadaBridge{
		logger: logger,
		svc:    svc,
		b:      b,
		client: client,
		siteID: siteID,
		cid:    cid,
	}
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (omb *OmadaBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	omb.logger.Error("received unsupported command - shouldn't happen")
	return nil, bridge.ErrUnsupportedCommand
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (omb *OmadaBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	omb.b.Config.Name = config.Name
	omb.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it queries
// the Omada API and returns the current state of all the connected devices.
func (omb *OmadaBridge) Refresh(ctx context.Context) error {
	trueArg := "true"
	req := &api.GetGridActiveClientsParams{
		Page:            1,
		PageSize:        50,
		FiltersWireless: &trueArg,
		SortsMac:        &trueArg,
	}
	resp, err := omb.client.GetGridActiveClientsWithResponse(context.Background(), omb.cid, omb.siteID, req)
	if err != nil {
		omb.logger.Error("unable to get status from API",
			zap.Error(err), zap.String("site_id", omb.siteID))
		return status.Error(codes.Internal, "unable to get status from API")
	}

	for _, activeClient := range *resp.JSON200.Result.Data {
		omb.svc.UpdateDevice(omClientInfoToDevice(&activeClient))
	}

	return nil
}

// Run begins the process of polling the API and reporting back the state.
func (omb *OmadaBridge) Run() {
	omb.Refresh(context.Background())

	refreshTimer := time.NewTicker(time.Minute * 1)
	for {
		select {
		case <-refreshTimer.C:
			err := omb.Refresh(context.Background())
			if err != nil {
				omb.logger.Error("unable to get status from API",
					zap.Error(err))
				continue
			} else {
				omb.logger.Debug("refreshed")
			}
		}
	}
}
