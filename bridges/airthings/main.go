package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/spf13/viper"

	"tinygo.org/x/bluetooth"

	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigName("airthings")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/etc/house")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	viper.SetDefault("bridge.refresh_interval", 300)
	viper.SetDefault("bridge.listen_port", 17002)

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

	var btAdapter *bluetooth.Adapter

	if viper.IsSet("bluetooth.adapter_id") {
		btAdapter = bluetooth.NewAdapter(viper.GetString("bluetooth.adapter_id"))
	} else {
		btAdapter = bluetooth.DefaultAdapter
	}

	err = btAdapter.Enable()
	if err != nil {
		logger.Fatal("unable to enable bt adapter", zap.Error(err))
	}

	sensorIDs := viper.GetIntSlice("sensor.ids")
	if len(sensorIDs) < 1 {
		logger.Fatal("sensor.ids must be set in the config")
	}

	svc := bridge.NewService(logger)

	ab := NewAirthingsBridge(logger, svc, btAdapter, sensorIDs)

	svc.RegisterHandler(ab, ab.b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Check for updates periodically
	go ab.Run(ctx)

	s := bridge.NewServer(logger, svc)
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
