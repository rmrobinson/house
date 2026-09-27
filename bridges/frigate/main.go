package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/google/uuid"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/bridges/frigate/frigate"
	"github.com/rmrobinson/house/configutil"
	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("bridge.refresh_interval", 60)
	viper.SetDefault("bridge.listen_port", 17008)

	configPath, err := configutil.FindConfigFile("frigate", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	ipAddr := viper.GetString("frigate.ip")
	port := viper.GetInt("frigate.port")
	proto := viper.GetString("frigate.proto")
	if len(ipAddr) < 1 {
		logger.Fatal("frigate.ip must be set in the config")
	}
	if len(proto) < 1 {
		proto = "http"
	}
	if port == 0 {
		// Default Frigate port
		port = 5000
	}

	frigateAPIEndpoint, err := url.Parse(fmt.Sprintf("%s://%s:%d", proto, ipAddr, port))
	if err != nil {
		logger.Fatal("provided api details aren't a valid url")
	}

	frigateClient := frigate.NewClient(logger, &http.Client{}, frigateAPIEndpoint)

	svc := bridge.NewService(logger)

	fb := NewFrigateBridge(logger, svc, frigateClient, ipAddr)

	var cameraConfigs []CameraConfig
	if err := viper.UnmarshalKey("frigate.cameras", &cameraConfigs); err != nil {
		logger.Fatal("unable to parse 'cameras' key from config")
	}

	fb.Setup(context.Background(), cameraConfigs)

	// Once we've successfully gotten the device state, register the handler and device with the service
	svc.RegisterHandler(fb, fb.b)

	// Check for updates periodically
	go fb.Run(context.Background())

	s, err := bridge.NewServer(logger, svc, bridge.TLSConfigFromViper())
	if err != nil {
		logger.Fatal("unable to create bridge server", zap.Error(err))
	}
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
