package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/rmrobinson/omada"

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
	viper.SetDefault("bridge.listen_port", 17007)

	configPath, err := configutil.FindConfigFile("omada", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	omIpAddr := viper.GetString("omada.ip")
	omPort := viper.GetInt("omada.port")
	omProto := viper.GetString("omada.proto")
	if len(omIpAddr) < 1 {
		logger.Fatal("omada.ip must be set in the config")
	}
	if len(omProto) < 1 {
		omProto = "http"
	}

	omClientID := viper.GetString("omada.oauth.client_id")
	if len(omClientID) < 1 {
		logger.Fatal("omada.oauth.client_id must be set in the config")
	}
	omClientSecret := viper.GetString("omada.oauth.client_secret")
	if len(omClientSecret) < 1 {
		logger.Fatal("omada.oauth.client_secret must be set in the config")
	}
	omID := viper.GetString("omada.oauth.cid")
	if len(omID) < 1 {
		logger.Fatal("omada.oauth.cid must be set in the config")
	}
	omInsecureTls := viper.GetBool("omada.allow_insecure_tls")
	omSiteID := viper.GetString("omada.site_id")

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: omInsecureTls, // self-hosted Omada SDN controllers use self-signed certs. If you have a proper cert, don't use this.
			},
		},
	}
	omadaClient := omada.NewClient(logger, fmt.Sprintf("%s://%s:%d", omProto, omIpAddr, omPort), omID, omClientID, omClientSecret, httpClient)

	// TODO: Create Kea client

	svc := bridge.NewService(logger)

	omb := NewOmadaBridge(logger, svc, omadaClient, omIpAddr, omPort, omSiteID, omID, configPath)

	// Once we've successfully gotten the device state, register the handler and device with the service
	svc.RegisterHandler(omb, omb.b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Check for updates periodically
	go omb.Run(ctx)

	s, err := bridge.NewServer(logger, svc, bridge.TLSConfigFromViper())
	if err != nil {
		logger.Fatal("unable to create bridge server", zap.Error(err))
	}
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
