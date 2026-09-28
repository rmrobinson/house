package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/configutil"
	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("bridge.refresh_interval", 1800)
	viper.SetDefault("bridge.listen_port", 17006)

	configPath, err := configutil.FindConfigFile("plex", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	pb := NewPlexBridge(logger, svc, p, configPath)

	svc.RegisterHandler(pb, pb.b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pb.Run(ctx)

	plexCallbackPort := viper.GetInt("plex.callback_port")
	if plexCallbackPort > 0 {
		http.HandleFunc("/", p.handleWebhook)

		logger.Info("listening for plex callbacks", zap.Int("port", plexCallbackPort))
		go http.ListenAndServe(fmt.Sprintf(":%d", plexCallbackPort), http.DefaultServeMux)
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
