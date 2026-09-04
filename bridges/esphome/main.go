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

	viper.SetConfigName("esphome")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/etc/house")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	viper.SetDefault("bridge.listen_port", 17010)

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

	var nodeConfigs []nodeConfig
	if err := viper.UnmarshalKey("esphome.nodes", &nodeConfigs); err != nil {
		logger.Fatal("unable to parse esphome.nodes config", zap.Error(err))
	}

	svc := bridge.NewService(logger)

	eb := NewEsphomeBridge(logger, svc, nodeConfigs)
	svc.RegisterHandler(eb, eb.Bridge())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eb.Start(ctx)

	s := bridge.NewServer(logger, svc)
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
