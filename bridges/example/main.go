package main

import (
	"context"
	"time"

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

	viper.SetConfigName("example")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	viper.SetDefault("bridge.listen_port", 17001)

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

	eb := NewExampleBridge(logger, svc)

	svc.RegisterHandler(eb, eb.b)

	go func() {
		// Use this to mimic a bridge which takes a bit of time to detect and connect
		time.Sleep(time.Second * 15)
		logger.Debug("bridge initialized")

		svc.UpdateDevice(eb.d1.toDevice())
		svc.UpdateDevice(eb.d2.toDevice())
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go eb.Run(ctx)

	s := bridge.NewServer(logger, svc)
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
