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
	viper.AddConfigPath("/etc/house")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	viper.SetDefault("bridge.listen_port", 17009)

	if err := viper.ReadInConfig(); err != nil {
		logger.Fatal("unable to read config", zap.Error(err))
	}

	var idsChanged bool
	if len(viper.GetString("bridge.id")) < 1 {
		bridgeID := uuid.New().String()

		logger.Info("config missing bridge id, saving new bridge id",
			zap.String("bridge_id", bridgeID))

		viper.Set("bridge.id", bridgeID)
		idsChanged = true
	}
	if len(viper.GetString("device.id")) < 1 {
		deviceID := uuid.New().String()

		logger.Info("config missing device id, saving new device id",
			zap.String("device_id", deviceID))

		viper.Set("device.id", deviceID)
		idsChanged = true
	}
	if idsChanged {
		err = viper.WriteConfig()
		if err != nil {
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

	s := bridge.NewServer(logger, svc)
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
