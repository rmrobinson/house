package facade

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/configutil"
	"github.com/rmrobinson/house/service/lib/grpcutil"
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
	// UpstreamBridges are the BridgeServices this facade aggregates.
	UpstreamBridges []UpstreamBridge
	// ClientTLS configures mutual TLS for every one of those upstream
	// connections (see NewFromConfig -> Facade.Connect). Nil means plaintext
	// gRPC.
	ClientTLS *grpcutil.ClientTLSConfig
}

// UpstreamBridge is one BridgeService this facade aggregates, as read from
// one entry of facade.bridges.
type UpstreamBridge struct {
	// Address is the host:port this facade dials.
	Address string `mapstructure:"address"`
	// ServerName overrides the hostname ClientTLS verifies this upstream's
	// certificate against - required whenever Address isn't the name the
	// upstream's certificate was actually issued for, e.g. dialing by IP, or
	// several bridges sharing a hostname like "localhost", while cert-agent
	// issues each bridge its own cert for "<name>.<host>.house.internal".
	// Ignored when ClientTLS is nil (plaintext gRPC).
	ServerName string `mapstructure:"server_name"`
}

// LoadConfig reads a Config from viper's bridge.*/facade.bridges keys -
// bridge.id is generated and persisted back into configPath (via
// configutil.PersistValue, not viper.WriteConfig - see that function's doc
// comment for why) on first run if left empty, the same generate-and-persist
// pattern any individual bridge's main.go follows for its own Bridge.Id.
// The caller is responsible for viper's config name/search path and for
// calling ReadConfig first (see cmd/bridgefacaded/main.go); configPath is
// the same file that was read, needed here only for the id persistence.
//
// listenPort is the port the caller's gRPC server is (or will be) listening
// on - combined with bridge.host to build the self address published as the
// Address of every device this facade proxies. It's taken as a parameter
// rather than its own config key because it must always match the actual
// listener; letting it be configured separately (as bridge.address, in an
// earlier version of this config) let the two silently drift, advertising
// an address nothing was actually listening on. Only the host can't be
// inferred - a process has no way to know which of its own addresses (LAN
// IP, DNS name, NAT'd address, ...) is the one a downstream client should
// actually dial.
//
// Returns (nil, nil) if facade.bridges is unset/empty, so a caller that only
// conditionally embeds a facade (see cmd/housed) can tell "no facade
// configured" apart from "facade configured but invalid" - bridgefacaded,
// which always embeds one, treats a nil Config as a fatal config error
// instead.
func LoadConfig(logger *zap.Logger, listenPort int, configPath string) (*Config, error) {
	var bridges []UpstreamBridge
	if err := viper.UnmarshalKey("facade.bridges", &bridges); err != nil {
		return nil, fmt.Errorf("unable to parse facade.bridges config: %w", err)
	}
	if len(bridges) < 1 {
		return nil, nil
	}

	if len(viper.GetString("bridge.id")) < 1 {
		bridgeID := uuid.New().String()
		logger.Info("config missing bridge id, saving new bridge id", zap.String("bridge_id", bridgeID))
		viper.Set("bridge.id", bridgeID)
		if err := configutil.PersistValue(configPath, "bridge.id", bridgeID); err != nil {
			return nil, fmt.Errorf("unable to write new config: %w", err)
		}
	}

	host := viper.GetString("bridge.host")
	if len(host) < 1 {
		return nil, errors.New("bridge.host is required: the host downstream clients use to reach this facade")
	}
	selfAddress := fmt.Sprintf("%s:%d", host, listenPort)

	// A facade listed as its own upstream would have it dial itself and
	// subscribe to its own StreamUpdates, which - even if it didn't just
	// hang waiting for a connection that can't complete until it already
	// has - would ingest its own present()-rewritten devices back into its
	// cache as if they came from a real upstream bridge. This only catches
	// an exact string match (e.g. a copy-pasted address); it can't detect
	// e.g. "localhost:X" aliasing "192.168.1.5:X".
	for _, b := range bridges {
		if b.Address == selfAddress {
			return nil, fmt.Errorf("facade.bridges cannot include this facade's own address (%s)", selfAddress)
		}
	}

	var clientTLS *grpcutil.ClientTLSConfig
	if certFile := viper.GetString("bridge.tls.cert_file"); len(certFile) > 0 {
		// The same cert/key this facade uses to authenticate itself as a
		// server (see cmd/bridgefacaded/main.go) doubles as its client
		// identity here - it's one principal ("this facade") either way,
		// and step-ca's default leaf certs carry both the serverAuth and
		// clientAuth EKUs. client_ca_file is step-ca's root either way too:
		// verifying an inbound caller's cert and verifying an upstream
		// bridge's cert both chain to the same private CA.
		keyFile := viper.GetString("bridge.tls.key_file")
		caFile := viper.GetString("bridge.tls.client_ca_file")
		if len(keyFile) < 1 || len(caFile) < 1 {
			return nil, errors.New("bridge.tls.cert_file is set; bridge.tls.key_file and bridge.tls.client_ca_file are required together with it")
		}

		clientTLS = &grpcutil.ClientTLSConfig{
			CertFile: certFile,
			KeyFile:  keyFile,
			CAFile:   caFile,
		}
	}

	return &Config{
		BridgeID:          viper.GetString("bridge.id"),
		BridgeName:        viper.GetString("bridge.name"),
		BridgeDescription: viper.GetString("bridge.description"),
		SelfAddress:       selfAddress,
		UpstreamBridges:   bridges,
		ClientTLS:         clientTLS,
	}, nil
}

// NewFromConfig builds a Facade from cfg and starts (ctx-scoped) connections
// to every upstream bridge in cfg.UpstreamBridges. Callers must register the
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

	f := New(logger, self, cfg.SelfAddress, cfg.ClientTLS)
	for _, b := range cfg.UpstreamBridges {
		logger.Info("connecting to upstream bridge", zap.String("address", b.Address), zap.String("server_name", b.ServerName))
		f.Connect(ctx, b.Address, b.ServerName)
	}
	return f
}
