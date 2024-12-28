package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/spf13/viper"

	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigName("roku")
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

	rb := NewRokuBridge(logger, svc)
	if err := rb.Refresh(context.Background()); err != nil {
		logger.Fatal("unable to refresh bridge", zap.Error(err))
	}

	// Once we've successfully gotten the device state, register the handler and device with the service
	svc.RegisterHandler(rb, rb.b)

	// Check for updates periodically
	runCtx, runCtxCancel := context.WithCancel(context.Background())
	defer runCtxCancel()

	go rb.Run(runCtx)

	s := bridge.NewServer(logger, svc)
	s.Serve()
}
