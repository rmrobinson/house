package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/google/uuid"
	airthings "github.com/rmrobinson/airthings-btle"
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

	sensorIDs := viper.GetIntSlice("sensor.ids")
	if len(sensorIDs) < 1 {
		logger.Fatal("sensor.ids must be set in the config")
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

	scanner := airthings.NewScanner(btAdapter)

	cb := NewAirthingsBridge(logger, svc, scanner, sensorIDs)

	// Once we've successfully gotten the device state, register the handler and device with the service
	svc.RegisterHandler(cb, cb.b)

	// Check for updates periodically
	go cb.Run(context.Background())

	s := bridge.NewServer(logger, svc)
	s.Serve()
}
