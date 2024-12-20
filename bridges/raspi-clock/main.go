package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/rafalop/sevensegment"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/bridge"
)

const i2cAddress = 0x70

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigName("raspi-clock")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			deviceID := uuid.New().String()
			bridgeID := uuid.New().String()

			logger.Info("config missing, saving new device and bridge ids",
				zap.String("bridge_id", bridgeID),
				zap.String("device_id", deviceID))

			viper.Set("bridge.id", bridgeID)
			viper.Set("device.id", deviceID)

			err = viper.WriteConfig()
			if err != nil {
				logger.Fatal("unable to write new config", zap.Error(err))
			}
		}
		logger.Fatal("unable to read config", zap.Error(err))
	}

	svc := bridge.NewService(logger)

	d := sevensegment.NewSevenSegment(i2cAddress)
	d.Clear()
	d.SetBrightness(0)

	c := NewClock(d)
	go c.Run(context.Background())

	_ = NewClockBridge(logger, svc, c)

	s := bridge.NewServer(logger, svc)
	s.Serve()
}
