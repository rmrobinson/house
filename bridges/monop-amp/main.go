package main

import (
	"context"

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

	viper.SetConfigName("monoprice-amp")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	viper.SetDefault("amp.usb.baud_rate", 9600)

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

	var inputs []inputDetails
	if err := viper.UnmarshalKey("amp.inputs", &inputs); err != nil {
		logger.Fatal("unable to parse 'inputs' key from config")
	}
	var speakers []speakerDetails
	if err := viper.UnmarshalKey("amp.speakers", &speakers); err != nil {
		logger.Fatal("unable to parse 'speakers' from config")
	}

	usbPath := viper.GetString("amp.usb.path")
	if len("usbPath") < 1 {
		logger.Fatal("must specify a usb path for the amp")
	}
	usbBaudRate := viper.GetInt("amp.usb.baud_rate")

	ampBridge := NewMonopriceAmpBridge(logger, svc, usbPath, usbBaudRate, inputs, speakers)

	if err := ampBridge.Start(context.Background()); err != nil {
		logger.Fatal("unable to start amplifier")
	}

	svc.RegisterHandler(ampBridge, ampBridge.b)

	s := bridge.NewServer(logger, svc)
	s.Serve()
}
