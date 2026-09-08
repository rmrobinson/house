package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/spf13/viper"

	"github.com/rmrobinson/house/bridges/lib/homekitctrl"
	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigName("ecobee")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/etc/house")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	viper.SetDefault("bridge.listen_port", 17011)
	viper.SetDefault("bridge.refresh_interval", 30)

	if err := viper.ReadInConfig(); err != nil {
		logger.Fatal("unable to read config", zap.Error(err))
	}

	if len(viper.GetString("bridge.id")) < 1 {
		bridgeID := uuid.New().String()

		logger.Info("config missing bridge id, saving new bridge id",
			zap.String("bridge_id", bridgeID))

		viper.Set("bridge.id", bridgeID)

		if err := viper.WriteConfig(); err != nil {
			logger.Fatal("unable to write new config", zap.Error(err))
		}
	}

	var cfg ecobeeConfig
	if err := viper.UnmarshalKey("ecobee", &cfg); err != nil {
		logger.Fatal("unable to parse ecobee config", zap.Error(err))
	}
	if len(cfg.PairingStore) < 1 {
		logger.Fatal("ecobee.pairing_store must be set in the config")
	}
	if len(cfg.AccessoryName) < 1 {
		cfg.AccessoryName = "ecobee"
	}
	if err := cfg.validate(); err != nil {
		logger.Fatal("invalid ecobee config", zap.Error(err))
	}

	store := homekitctrl.NewFileStore(cfg.PairingStore)

	svc := bridge.NewService(logger)

	eb := NewEcobeeBridge(logger, svc, store, cfg)
	svc.RegisterHandler(eb, eb.Bridge())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go eb.Run(ctx)

	s := bridge.NewServer(logger, svc)
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
