package main

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/mdlayher/apcupsd"
	"github.com/spf13/viper"

	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigName("apc-ups")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	viper.SetDefault("bridge.refresh_interval", 60)
	viper.SetDefault("bridge.listen_port", 17003)

	if err := viper.ReadInConfig(); err != nil {
		logger.Fatal("unable to read config", zap.Error(err))
	}

	if len(viper.GetString("bridge.id")) < 1 {
		bridgeID := uuid.New().String()

		logger.Info("config missing bridge id, saving new bridge id",
			zap.String("bridge_id", bridgeID))

		viper.Set("bridge.id", bridgeID)

		err = viper.WriteConfig()
		if err != nil {
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

	s := bridge.NewServer(logger, svc)
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
