package main

import (
	"bytes"
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/spf13/viper"

	"github.com/rmrobinson/house/service/bridge"
	"github.com/rmrobinson/house/service/lib/configutil"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("amp.usb.baud_rate", 9600)
	viper.SetDefault("bridge.listen_port", 17016)
	viper.SetDefault("bridge.refresh_interval", 60)

	configPath, err := configutil.FindConfigFile("monoprice-amp", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	var inputs []inputDetails
	if err := viper.UnmarshalKey("amp.inputs", &inputs); err != nil {
		logger.Fatal("unable to parse 'amp.inputs' key from config", zap.Error(err))
	}
	var speakers []speakerDetails
	if err := viper.UnmarshalKey("amp.speakers", &speakers); err != nil {
		logger.Fatal("unable to parse 'amp.speakers' from config", zap.Error(err))
	}

	usbPath := viper.GetString("amp.usb.path")
	if len(usbPath) < 1 {
		logger.Fatal("must specify a usb path for the amp")
	}
	usbBaudRate := viper.GetInt("amp.usb.baud_rate")

	ampBridge := NewMonopriceAmpBridge(logger, svc, configPath, usbPath, usbBaudRate, inputs, speakers)

	if err := ampBridge.Start(context.Background()); err != nil {
		logger.Fatal("unable to start amplifier", zap.Error(err))
	}
	defer ampBridge.Close()

	svc.RegisterHandler(ampBridge, ampBridge.b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ampBridge.Run(ctx, time.Second*time.Duration(viper.GetInt("bridge.refresh_interval")))

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
