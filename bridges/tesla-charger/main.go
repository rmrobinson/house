package main

import (
	"net/http"

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

	viper.SetConfigName("tesla-charger")
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

	chargerIP := viper.GetString("charger.ip")
	if len(chargerIP) < 1 {
		logger.Fatal("charger.ip must be set in the config")
	}

	charger := NewCharger(logger, chargerIP, &http.Client{})
	cb := NewChargerBridge(logger, svc, charger)

	state, err := charger.State()
	if err != nil {
		logger.Fatal("unable to get state from charger", zap.Error(err))
	}

	// Once we've successfully gotten the device state, register the handler and device with the service
	svc.RegisterHandler(cb, cb.b)
	svc.UpdateDevice(state.toDevice())

	// Check for updates periodically
	go cb.Run()

	s := bridge.NewServer(logger, svc)
	s.Serve()
}
