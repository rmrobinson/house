package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigName("plex")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	viper.SetDefault("bridge.refresh_interval", 1800)
	viper.SetDefault("bridge.listen_port", 17006)

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			bridgeID := uuid.New().String()

			logger.Info("config missing, saving new device and bridge ids",
				zap.String("bridge_id", bridgeID))

			viper.Set("bridge.id", bridgeID)

			err = viper.WriteConfig()
			if err != nil {
				logger.Fatal("unable to write new config", zap.Error(err))
			}
		}
		logger.Fatal("unable to read config", zap.Error(err))
	}

	plexURL := viper.GetString("plex.server_url")
	plexAPIKey := viper.GetString("plex.api_key")
	if len(plexAPIKey) < 1 {
		logger.Fatal("no plex API key specified")
	}

	svc := bridge.NewService(logger)

	p := NewPlex(logger, svc, plexURL, plexAPIKey)

	if err := p.Start(context.Background()); err != nil {
		logger.Fatal("unable to start plex", zap.Error(err))
	}

	go func() {
		for {
			time.Sleep(time.Second * time.Duration(viper.GetInt("bridge.refresh_interval")))
			if err := p.Refresh(context.Background()); err != nil {
				logger.Error("unable to refresh plex state", zap.Error(err))
			}
		}
	}()

	pb := NewPlexBridge(logger, svc, p)

	svc.RegisterHandler(pb, pb.b)

	plexCallbackPort := viper.GetInt("plex.callback_port")
	if plexCallbackPort > 0 {
		http.HandleFunc("/", p.handleWebhook)

		logger.Info("listening for plex callbacks", zap.Int("port", plexCallbackPort))
		go http.ListenAndServe(fmt.Sprintf(":%d", plexCallbackPort), http.DefaultServeMux)
	}

	s := bridge.NewServer(logger, svc)
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
