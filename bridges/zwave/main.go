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
		viper.SetDefault("bridge.listen_port", 17014)

	configPath, err := configutil.FindConfigFile("zwave", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	var cfg zwaveConfig
	if err := viper.UnmarshalKey("zwave", &cfg); err != nil {
		logger.Fatal("unable to parse zwave config", zap.Error(err))
	}

	svc := bridge.NewService(logger)

	zb, err := NewZwaveBridge(logger, svc, cfg)
	if err != nil {
		logger.Fatal("unable to create zwave bridge", zap.Error(err))
	}
	svc.RegisterHandler(zb, zb.Bridge())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := zb.Start(ctx); err != nil {
		logger.Fatal("unable to connect to mqtt broker", zap.Error(err))
	}

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
