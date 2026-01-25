package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/google/uuid"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/bridges/frigate/frigate"
	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigName("frigate")
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

	ipAddr := viper.GetString("frigate.ip")
	port := viper.GetInt("frigate.port")
	proto := viper.GetString("frigate.proto")
	if len(ipAddr) < 1 {
		logger.Fatal("frigate.ip must be set in the config")
	}
	if len(proto) < 1 {
		proto = "http"
	}
	if port == 0 {
		// Default Frigate port
		port = 5000
	}

	frigateAPIEndpoint, err := url.Parse(fmt.Sprintf("%s://%s:%d", proto, ipAddr, port))
	if err != nil {
		logger.Fatal("provided api details aren't a valid url")
	}

	frigateClient := frigate.NewClient(logger, &http.Client{}, frigateAPIEndpoint)

	fb := NewFrigateBridge(logger, svc, frigateClient, ipAddr)

	// Once we've successfully gotten the device state, register the handler and device with the service
	svc.RegisterHandler(fb, fb.b)

	fb.Setup(context.Background(), nil)

	// Check for updates periodically
	go fb.Run(context.Background())

	s := bridge.NewServer(logger, svc)
	s.Serve()
}
