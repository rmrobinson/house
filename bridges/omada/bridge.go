package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/rmrobinson/omada"
	"github.com/rmrobinson/omada/api"
	omapi "github.com/rmrobinson/omada/api"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/lib/configutil"
	"github.com/rmrobinson/house/service/bridge"
)

// omClientInfoToDevice returns nil if s has no MAC address - clientMac is the device ID, so
// without it there's nothing usable to build.
func omClientInfoToDevice(s *omapi.ClientInfo) *device.Device {
	if s.Mac == nil {
		return nil
	}

	cleanMAC := strings.ReplaceAll(*s.Mac, "-", ":")
	cleanMAC = strings.ToUpper(cleanMAC)

	state := &trait.NetworkPresence_State{
		HardwareAddress: cleanMAC,
		IpAddresses:     []string{},
	}

	if s.DeviceType != nil {
		state.DeviceCategory = *s.DeviceType
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

	d := &device.Device{
		Id: *s.Mac,
		Details: &device.Device_ConnectedDevice{
			ConnectedDevice: &device.ConnectedDevice{
				NetworkPresence: &trait.NetworkPresence{
					State: state,
				},
			},
		},
		// Everything GetGridActiveClients returns is, by definition, currently connected -
		// Refresh is responsible for flipping this to false once a MAC stops appearing there.
		Address: &device.Device_Address{IsReachable: true},
	}

	if s.LastSeen != nil {
		d.LastSeen = timestamppb.New(time.Unix(*s.LastSeen, 0))
	}
	if s.Ip != nil {
		d.Address.Address = *s.Ip
	}
	if s.HostName != nil {
		d.Config = &device.Device_Config{Name: *s.HostName}
	}

	return d
}

// OmadaBridge
type OmadaBridge struct {
	logger     *zap.Logger
	svc        *bridge.Service
	b          *api2.Bridge
	configPath string

	client *omada.Client
	cid    string
	siteID string

	mu sync.Mutex
	// lastDevices holds the most recently published device for every MAC this bridge has ever
	// seen, keyed by device ID. Refresh needs this itself (bridge.Service's own device map is
	// unexported) to notice a client that's dropped out of the current "active clients" page -
	// without it a device that leaves the network would never be updated again and would stay
	// reported reachable forever.
	lastDevices map[string]*device.Device
}

// NewOmadaBridge creates a new Omada bridge from the supplied client.
// configPath is the on-disk config file SetBridgeConfig persists name/
// description edits into - see configutil.PersistValue's doc comment for
// why that's not viper.WriteConfig: this config has a !secret-tagged
// oauth.client_secret resolved into viper's live state, and WriteConfig
// would dump that resolved value straight back into the tracked file.
func NewOmadaBridge(logger *zap.Logger, svc *bridge.Service, client *omada.Client, omadaIPAddr string, omadaPort int, siteID string, cid string, configPath string) *OmadaBridge {
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
		logger:      logger,
		svc:         svc,
		b:           b,
		configPath:  configPath,
		client:      client,
		siteID:      siteID,
		cid:         cid,
		lastDevices: make(map[string]*device.Device),
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
	return configutil.PersistValues(omb.configPath,
		configutil.KeyValue{KeyPath: "bridge.name", Value: config.Name},
		configutil.KeyValue{KeyPath: "bridge.description", Value: config.Description},
	)
}

// ProcessCommandAsync is present to conform to the bridge.Handler interface. This bridge has no
// device traits eligible for asynchronous commands, so it always returns ErrAsyncCommandsNotSupported.
func (omb *OmadaBridge) ProcessCommandAsync(ctx context.Context, cmd *command.Command) error {
	return bridge.ErrAsyncCommandsNotSupported
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it queries
// the Omada API and returns the current state of all the connected devices.
func (omb *OmadaBridge) Refresh(ctx context.Context) error {
	trueArg := "true"
	// NOTE: FiltersWireless restricts this to wireless clients only - wired clients are never
	// reported by this bridge. Leaving this as-is matches the bridge's existing (pre-existing,
	// not changed here) behaviour; revisit if wired client visibility is wanted too.
	const pageSize = int32(50)

	seen := make(map[string]bool)
	page := int32(1)
	for {
		req := &api.GetGridActiveClientsParams{
			Page:            page,
			PageSize:        pageSize,
			FiltersWireless: &trueArg,
			SortsMac:        &trueArg,
		}
		resp, err := omb.client.GetGridActiveClientsWithResponse(context.Background(), omb.cid, omb.siteID, req)
		if err != nil {
			omb.logger.Error("unable to get status from API",
				zap.Error(err), zap.String("site_id", omb.siteID))
			omb.markAllUnreachable()
			return status.Error(codes.Internal, "unable to get status from API")
		}
		if resp.JSON200 == nil || resp.JSON200.Result == nil {
			omb.logger.Error("unable to get status from API: empty response",
				zap.String("site_id", omb.siteID))
			omb.markAllUnreachable()
			return status.Error(codes.Internal, "unable to get status from API")
		}

		var clients []omapi.ClientInfo
		if resp.JSON200.Result.Data != nil {
			clients = *resp.JSON200.Result.Data
		}

		for i := range clients {
			d := omClientInfoToDevice(&clients[i])
			if d == nil {
				omb.logger.Warn("skipping client with no mac address")
				continue
			}
			seen[d.Id] = true

			omb.mu.Lock()
			omb.lastDevices[d.Id] = d
			omb.mu.Unlock()

			omb.svc.UpdateDevice(d)
		}

		if int32(len(clients)) < pageSize {
			break
		}
		page++
	}

	// Anything published on a previous poll but not seen in this one has left the network -
	// republish it as unreachable rather than calling svc.RemoveDevice, since it may be linked
	// to a room in housed and should come back as the same device if the client reconnects.
	omb.mu.Lock()
	var goneDevices []*device.Device
	for id, d := range omb.lastDevices {
		if seen[id] || !d.GetAddress().GetIsReachable() {
			continue
		}
		gone := proto.Clone(d).(*device.Device)
		gone.Address.IsReachable = false
		omb.lastDevices[id] = gone
		goneDevices = append(goneDevices, gone)
	}
	omb.mu.Unlock()

	for _, d := range goneDevices {
		omb.svc.UpdateDevice(d)
	}

	return nil
}

// markAllUnreachable flips every known device's reachability off in place, preserving its last
// known state otherwise. Called when the Omada API itself couldn't be reached at all, so none of
// the currently-known clients' state can be trusted.
func (omb *OmadaBridge) markAllUnreachable() {
	omb.mu.Lock()
	defer omb.mu.Unlock()

	for id, d := range omb.lastDevices {
		if !d.GetAddress().GetIsReachable() {
			continue
		}
		gone := proto.Clone(d).(*device.Device)
		gone.Address.IsReachable = false
		omb.lastDevices[id] = gone
		omb.svc.UpdateDevice(gone)
	}
}

// Run begins the process of polling the API and reporting back the state.
func (omb *OmadaBridge) Run(ctx context.Context) {
	omb.Refresh(ctx)

	refreshTimer := time.NewTicker(time.Second * time.Duration(viper.GetInt("bridge.refresh_interval")))
	omb.logger.Info("beginning refresh loop", zap.Int("refresh_interval", viper.GetInt("bridge.refresh_interval")))
	for {
		select {
		case <-refreshTimer.C:
			if err := omb.Refresh(ctx); err != nil {
				omb.logger.Error("unable to get status from API",
					zap.Error(err))
				continue
			}
			omb.logger.Debug("refreshed")
		case <-ctx.Done():
			omb.logger.Info("run context cancelled")
			return
		}
	}
}
