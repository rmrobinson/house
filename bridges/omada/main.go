package main

import (
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/rmrobinson/omada"

	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigName("omada")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

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

	// Create Kea client

	omb := NewOmadaBridge(logger, svc, omadaClient, omIpAddr, omPort, omSiteID, omID)

	// Once we've successfully gotten the device state, register the handler and device with the service
	svc.RegisterHandler(omb, omb.b)

	// Check for updates periodically
	go omb.Run()

	s := bridge.NewServer(logger, svc)
	s.Serve()
}
