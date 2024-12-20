package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/spf13/viper"

	airthings "github.com/rmrobinson/airthings-btle"

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

	sensorID := viper.GetInt("sensor.id")
	if sensorID < 1 {
		logger.Fatal("sensor.id must be set in the config")
	}

	btAdapter := bluetooth.DefaultAdapter

	err = btAdapter.Enable()
	if err != nil {
		logger.Fatal("unable to enable bt adapter", zap.Error(err))
	}

	scanner := airthings.NewScanner(btAdapter)

	logger.Debug("beginning bluetooth scan")
	sensor, err := scanner.FindSensor(context.Background(), viper.GetInt("sensor.id"))
	if err != nil {
		logger.Fatal("unable to find sensor", zap.Error(err))
	}
	if sensor == nil {
		logger.Fatal("empty sensor")
	}

	cb := NewAirthingsBridge(logger, svc, sensor)

	err = sensor.Refresh()
	if err != nil {
		logger.Fatal("unable to get state from sensor", zap.Error(err))
	}

	// Once we've successfully gotten the device state, register the handler and device with the service
	svc.RegisterHandler(cb, cb.b)
	svc.UpdateDevice(sensorToDevice(sensor))

	// Check for updates periodically
	go cb.Run()

	s := bridge.NewServer(logger, svc)
	s.Serve()
}
