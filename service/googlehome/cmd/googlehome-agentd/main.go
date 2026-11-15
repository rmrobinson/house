// googlehome-agentd is the local half of the Google Smart Home Action
// integration: it runs on the home network, dials out to googlehomed's
// RelayService (never the reverse - see service/googlehome/relay's package
// doc comment), and translates between it and a BridgeService endpoint
// (housed, bridgefacaded, or a single bridge - see service/googlehome/agent).
//
// googlehome.relay_addr/googlehome.agent_id are required: agent_id must
// match the single agent_user_id googlehomed is configured to return from
// Validate (see cmd/googlehomed's staticTokenValidator) - house is a
// single-building system, so there is exactly one of each.
//
// googlehome.tls.* configures mutual TLS for dialing --bridge-addr,
// --relay-addr, and --house-addr alike - same one-identity-for-every-role
// convention as policyd's --tls-cert/--tls-key/--tls-ca (see
// service/policy/cmd/policyd/main.go).
//
// googlehome.house_addr/building_id are both optional together - unset,
// SYNC just never sets a device's RoomHint (see agent.Agent's own doc
// comment on why: a standalone bridgefacaded, unlike housed, has no
// HouseService to ask at all).
package main

import (
	"bytes"
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/googlehome/agent"
	"github.com/rmrobinson/house/service/googlehome/relay"
	"github.com/rmrobinson/house/service/lib/configutil"
	"github.com/rmrobinson/house/service/lib/grpcutil"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")

	configPath, err := configutil.FindConfigFile("googlehome-agentd", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
	if err != nil {
		logger.Fatal("unable to find config", zap.Error(err))
	}
	resolved, err := configutil.ResolveSecrets(configPath)
	if err != nil {
		logger.Fatal("unable to resolve config secrets", zap.Error(err))
	}
	if err := viper.ReadConfig(bytes.NewReader(resolved)); err != nil {
		logger.Fatal("unable to read config", zap.Error(err))
	}

	bridgeAddr := viper.GetString("googlehome.bridge_addr")
	relayAddr := viper.GetString("googlehome.relay_addr")
	agentID := viper.GetString("googlehome.agent_id")
	if bridgeAddr == "" || relayAddr == "" || agentID == "" {
		logger.Fatal("googlehome.bridge_addr, googlehome.relay_addr and googlehome.agent_id are all required")
	}

	var tlsCfg *grpcutil.ClientTLSConfig
	if certFile := viper.GetString("googlehome.tls.cert_file"); certFile != "" {
		keyFile := viper.GetString("googlehome.tls.key_file")
		caFile := viper.GetString("googlehome.tls.ca_file")
		if keyFile == "" || caFile == "" {
			logger.Fatal("googlehome.tls.cert_file is set; googlehome.tls.key_file and googlehome.tls.ca_file are required together with it")
		}
		tlsCfg = &grpcutil.ClientTLSConfig{
			CertFile:   certFile,
			KeyFile:    keyFile,
			CAFile:     caFile,
			ServerName: viper.GetString("googlehome.tls.server_name"),
		}
	}

	relayConn, err := grpcutil.Dial(relayAddr, tlsCfg)
	if err != nil {
		logger.Fatal("unable to dial relay", zap.String("address", relayAddr), zap.Error(err))
	}
	defer relayConn.Close()
	relayClient := relay.NewRelayServiceClient(relayConn)

	var houseClient api2.HouseServiceClient
	buildingID := viper.GetString("googlehome.building_id")
	if houseAddr := viper.GetString("googlehome.house_addr"); houseAddr != "" {
		if buildingID == "" {
			logger.Fatal("googlehome.house_addr is set; googlehome.building_id is required together with it")
		}
		houseConn, err := grpcutil.Dial(houseAddr, tlsCfg)
		if err != nil {
			logger.Fatal("unable to dial house service", zap.String("address", houseAddr), zap.Error(err))
		}
		defer houseConn.Close()
		houseClient = api2.NewHouseServiceClient(houseConn)
	}

	a := agent.New(logger, bridgeAddr, tlsCfg, relayClient, agentID, houseClient, buildingID)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("running googlehome agent", zap.String("bridge_addr", bridgeAddr), zap.String("relay_addr", relayAddr), zap.String("agent_id", agentID))
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Fatal("agent run error", zap.Error(err))
	}
}
