package main

import (
	"bytes"
	"context"

	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/spf13/viper"

	"github.com/rmrobinson/house/service/lib/configutil"
	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
		viper.SetDefault("bridge.listen_port", 17015)

	configPath, err := configutil.FindConfigFile("nanoleaf", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	var deviceConfigs []deviceConfig
	if err := viper.UnmarshalKey("nanoleaf.devices", &deviceConfigs); err != nil {
		logger.Fatal("unable to parse nanoleaf.devices config", zap.Error(err))
	}

	svc := bridge.NewService(logger)

	nb := NewNanoleafBridge(logger, svc, deviceConfigs)
	svc.RegisterHandler(nb, nb.Bridge())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	nb.Start(ctx)

	tlsCfg, err := bridge.TLSConfigFromViper()
	if err != nil {
		logger.Fatal("invalid bridge.tls config", zap.Error(err))
	}
	s, err := bridge.NewServer(logger, svc, tlsCfg)
	if err != nil {
		logger.Fatal("unable to create bridge server", zap.Error(err))
	}
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
