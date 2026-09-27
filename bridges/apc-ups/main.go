package main

import (
	"bytes"
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/mdlayher/apcupsd"
	"github.com/spf13/viper"

	"github.com/rmrobinson/house/configutil"
	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("bridge.refresh_interval", 60)
	viper.SetDefault("bridge.listen_port", 17003)

	configPath, err := configutil.FindConfigFile("apc-ups", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	if len(viper.GetString("bridge.id")) < 1 {
		bridgeID := uuid.New().String()

		logger.Info("config missing bridge id, saving new bridge id",
			zap.String("bridge_id", bridgeID))

		viper.Set("bridge.id", bridgeID)

		if err := configutil.PersistValue(configPath, "bridge.id", bridgeID); err != nil {
			logger.Fatal("unable to write new config", zap.Error(err))
		}
	}

	ipAddr := viper.GetString("ups.ip")
	port := viper.GetInt("ups.port")
	proto := viper.GetString("ups.proto")
	if len(ipAddr) < 1 {
		logger.Fatal("ups.ip must be set in the config")
	}
	if !viper.IsSet("ups.port") {
		logger.Fatal("ups.port must be set in the config")
	}
	if len(proto) < 1 {
		proto = "tcp"
	}

	apcUPSClient, err := apcupsd.Dial(proto, fmt.Sprintf("%s:%d", ipAddr, port))
	if err != nil {
		logger.Fatal("unable to connect to ups", zap.Error(err))
	}

	svc := bridge.NewService(logger)

	upsb := NewAPCUPSBridge(logger, svc, apcUPSClient, ipAddr, port)

	// Once we've successfully gotten the device state, register the handler and device with the service
	svc.RegisterHandler(upsb, upsb.b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Check for updates periodically
	go upsb.Run(ctx)

	s, err := bridge.NewServer(logger, svc, bridge.TLSConfigFromViper())
	if err != nil {
		logger.Fatal("unable to create bridge server", zap.Error(err))
	}
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
