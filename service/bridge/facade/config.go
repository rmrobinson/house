package facade

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
)

// Config is a BridgeService facade's configuration, as read from viper by
// LoadConfig - shared by every process that can embed a facade (see
// cmd/bridgefacaded, which does nothing but embed one, and
// service/house/cmd/housed, which can optionally embed one alongside
// HouseService).
type Config struct {
	BridgeID          string
	BridgeName        string
	BridgeDescription string
	// SelfAddress is the network address downstream clients use to reach
	// this facade - published as the Address of every device it proxies.
	SelfAddress string
	// UpstreamAddrs are the BridgeService addresses this facade aggregates.
	UpstreamAddrs []string
}

// LoadConfig reads a Config from viper's bridge.*/facade.bridges keys -
// bridge.id is generated and persisted back via viper.WriteConfig on first
// run if left empty, the same generate-and-persist pattern any individual
// bridge's main.go follows for its own Bridge.Id. The caller is responsible
// for viper's config name/search path and for calling ReadInConfig first
// (see cmd/bridgefacaded/main.go).
//
// Returns (nil, nil) if facade.bridges is unset/empty, so a caller that only
// conditionally embeds a facade (see cmd/housed) can tell "no facade
// configured" apart from "facade configured but invalid" - bridgefacaded,
// which always embeds one, treats a nil Config as a fatal config error
// instead.
func LoadConfig(logger *zap.Logger) (*Config, error) {
	var addrs []string
	if err := viper.UnmarshalKey("facade.bridges", &addrs); err != nil {
		return nil, fmt.Errorf("unable to parse facade.bridges config: %w", err)
	}
	if len(addrs) < 1 {
		return nil, nil
	}

	if len(viper.GetString("bridge.id")) < 1 {
		bridgeID := uuid.New().String()
		logger.Info("config missing bridge id, saving new bridge id", zap.String("bridge_id", bridgeID))
		viper.Set("bridge.id", bridgeID)
		if err := viper.WriteConfig(); err != nil {
			return nil, fmt.Errorf("unable to write new config: %w", err)
		}
	}

	selfAddress := viper.GetString("bridge.address")
	if len(selfAddress) < 1 {
		return nil, errors.New("bridge.address is required: the address downstream clients use to reach this facade")
	}

	// A facade listed as its own upstream would have it dial itself and
	// subscribe to its own StreamUpdates, which - even if it didn't just
	// hang waiting for a connection that can't complete until it already
	// has - would ingest its own present()-rewritten devices back into its
	// cache as if they came from a real upstream bridge. This only catches
	// an exact string match (e.g. a copy-pasted address); it can't detect
	// e.g. "localhost:X" aliasing "192.168.1.5:X".
	for _, addr := range addrs {
		if addr == selfAddress {
			return nil, fmt.Errorf("facade.bridges cannot include this facade's own address (bridge.address: %s)", selfAddress)
		}
	}

	return &Config{
		BridgeID:          viper.GetString("bridge.id"),
		BridgeName:        viper.GetString("bridge.name"),
		BridgeDescription: viper.GetString("bridge.description"),
		SelfAddress:       selfAddress,
		UpstreamAddrs:     addrs,
	}, nil
}

// NewFromConfig builds a Facade from cfg and starts (ctx-scoped) connections
// to every upstream bridge in cfg.UpstreamAddrs. Callers must register the
// returned Facade as a BridgeServiceServer themselves - it holds no opinion
// on what port/listener it's served from, since an embedding caller (see
// cmd/housed) may share one with other registered services.
func NewFromConfig(ctx context.Context, logger *zap.Logger, cfg *Config) *Facade {
	self := &api2.Bridge{
		Id:           cfg.BridgeID,
		IsReachable:  true,
		ModelId:      "HouseBridgeFacade",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        cfg.BridgeName,
			Description: cfg.BridgeDescription,
		},
	}

	f := New(logger, self, cfg.SelfAddress)
	for _, addr := range cfg.UpstreamAddrs {
		logger.Info("connecting to upstream bridge", zap.String("address", addr))
		f.Connect(ctx, addr)
	}
	return f
}
