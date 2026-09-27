package main

import (
	"bytes"
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/spf13/viper"

	"github.com/rmrobinson/house/configutil"
	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
		viper.SetDefault("bridge.listen_port", 17001)

	configPath, err := configutil.FindConfigFile("example", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	s, err := bridge.NewServer(logger, svc, bridge.TLSConfigFromViper())
	if err != nil {
		logger.Fatal("unable to create bridge server", zap.Error(err))
	}
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
