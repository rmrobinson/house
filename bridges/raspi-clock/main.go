package main

import (
	"bytes"
	"context"

	"github.com/google/uuid"
	"github.com/rafalop/sevensegment"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/lib/configutil"
	"github.com/rmrobinson/house/service/bridge"
)

const i2cAddress = 0x70

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("bridge.listen_port", 17009)

	configPath, err := configutil.FindConfigFile("raspi-clock", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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
	if len(viper.GetString("device.id")) < 1 {
		deviceID := uuid.New().String()

		logger.Info("config missing device id, saving new device id",
			zap.String("device_id", deviceID))

		viper.Set("device.id", deviceID)
		if err := configutil.PersistValue(configPath, "device.id", deviceID); err != nil {
			logger.Fatal("unable to write new config", zap.Error(err))
		}
	}

	svc := bridge.NewService(logger)

	d := sevensegment.NewSevenSegment(i2cAddress)
	d.Clear()
	d.SetBrightness(0)

	c := NewClock(d)
	go c.Run(context.Background())

	_ = NewClockBridge(logger, svc, c)

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
