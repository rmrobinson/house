package main

import (
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

	svc := bridge.NewService(logger)

	ipAddr := viper.GetString("ups.ip")
	port := viper.GetInt("ups.port")
	proto := viper.GetString("ups.proto")
	if len(ipAddr) < 1 {
		logger.Fatal("ups.ip must be set in the config")
	}
	if len(proto) < 1 {
		proto = "tcp"
	}

	apcUPSClient, err := apcupsd.Dial(proto, fmt.Sprintf("%s:%d", ipAddr, port))
	if err != nil {
		logger.Fatal("unable to connect to ups", zap.Error(err))
	}

	cb := NewAPCUPSBridge(logger, svc, apcUPSClient, ipAddr, port)

	status, err := apcUPSClient.Status()
	if err != nil {
		logger.Fatal("unable to get status from UPS", zap.Error(err))
	}

	// Once we've successfully gotten the device state, register the handler and device with the service
	svc.RegisterHandler(cb, cb.b)
	svc.UpdateDevice(statusToDevice(status))

	// Check for updates periodically
	go cb.Run()

	s := bridge.NewServer(logger, svc)
	s.Serve()
}
