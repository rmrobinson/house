package main

import (
	"bytes"
	"context"

	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/spf13/viper"

	"tinygo.org/x/bluetooth"

	"github.com/rmrobinson/house/configutil"
	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("bridge.refresh_interval", 300)
	viper.SetDefault("bridge.listen_port", 17002)

	configPath, err := configutil.FindConfigFile("airthings", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
	if err != nil {
		logger.Fatal("unable to find config", zap.Error(err))
	}
	resolved, err := configutil.ResolveSecrets(configPath)
	if err != nil {
		logger.Fatal("unable to resolve config secrets", zap.Error(err))
	}
	if err := viper.ReadConfig(bytes.NewReader(resolved)); err != nil {
		logger.Fatal("unable to read config", zap.Error(err))
	}

	if len(viper.GetString("bridge.id")) < 1 {
		bridgeID := uuid.New().String()

		logger.Info("config missing bridge id, saving new bridge id",
			zap.String("bridge_id", bridgeID))

		viper.Set("bridge.id", bridgeID)

		if err := configutil.PersistValue(configPath, "bridge.id", bridgeID); err != nil {
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

	s, err := bridge.NewServer(logger, svc, bridge.TLSConfigFromViper())
	if err != nil {
		logger.Fatal("unable to create bridge server", zap.Error(err))
	}
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
