// googlehomed is the public-facing half of the Google Smart Home Action
// integration: it hosts the fulfillment webhook Google's Smart Home Action
// SDK calls (SYNC/QUERY/EXECUTE/DISCONNECT), calls HomeGraph to push state
// updates and request re-syncs, and exposes RelayService so
// googlehome-agentd (running alongside housed, on the home network) can
// dial in and register - see service/googlehome/relay's package doc
// comment for why the protocol is shaped this way.
//
// googlehome.fulfillment_listen_port serves plain HTTP: this process is
// expected to sit behind a reverse proxy (Caddy, with ACME) that terminates
// the publicly-trusted TLS certificate Google requires - the same pattern
// house-config already uses for its other public-facing services. It does
// not attempt Let's Encrypt/autocert itself the way the nerves precedent
// did.
//
// googlehome.relay_tls.* configures mutual TLS for the RelayService
// listener that googlehome-agentd dials into - this one isn't fronted by
// Caddy (it's a gRPC stream, not a browser-facing endpoint), so it uses the
// same cert-agent-issued mTLS convention as every other internal gRPC
// service in this repo.
//
// googlehome.homegraph.creds_file must point at a Google service account
// key with HomeGraph API access, configured in the Actions console project
// this integration is registered under.
package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/api/homegraph/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"

	action "github.com/rmrobinson/google-smart-home-action-go"

	"github.com/rmrobinson/house/service/googlehome/relay"
	"github.com/rmrobinson/house/service/lib/configutil"
	"github.com/rmrobinson/house/service/lib/grpcutil"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("googlehome.fulfillment_listen_port", 8090)
	viper.SetDefault("googlehome.relay_listen_port", 17060)

	configPath, err := configutil.FindConfigFile("googlehomed", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	agentUserID := viper.GetString("googlehome.agent_user_id")
	authToken := viper.GetString("googlehome.auth_token")
	credsFile := viper.GetString("googlehome.homegraph.creds_file")
	if agentUserID == "" || authToken == "" || credsFile == "" {
		logger.Fatal("googlehome.agent_user_id, googlehome.auth_token and googlehome.homegraph.creds_file are all required")
	}

	ctx := context.Background()
	hgService, err := homegraph.NewService(ctx, option.WithCredentialsFile(credsFile))
	if err != nil {
		logger.Fatal("unable to initialize homegraph client", zap.Error(err))
	}

	validator := &staticTokenValidator{token: authToken, agentUserID: agentUserID}
	providerSvc := relay.NewProviderService(logger)
	actionSvc := action.NewService(logger, validator, providerSvc, hgService)
	relayAPI := relay.NewAPI(logger, providerSvc, actionSvc)

	mux := http.NewServeMux()
	mux.HandleFunc(action.GoogleFulfillmentPath, actionSvc.GoogleFulfillmentHandler)

	fulfillmentPort := viper.GetInt("googlehome.fulfillment_listen_port")
	httpServer := &http.Server{Addr: fmt.Sprintf(":%d", fulfillmentPort), Handler: mux}
	go func() {
		logger.Info("serving google fulfillment webhook", zap.Int("port", fulfillmentPort))
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("fulfillment http server error", zap.Error(err))
		}
	}()

	var opts []grpc.ServerOption
	if certFile := viper.GetString("googlehome.relay_tls.cert_file"); certFile != "" {
		keyFile := viper.GetString("googlehome.relay_tls.key_file")
		caFile := viper.GetString("googlehome.relay_tls.client_ca_file")
		if keyFile == "" || caFile == "" {
			logger.Fatal("googlehome.relay_tls.cert_file is set; googlehome.relay_tls.key_file and googlehome.relay_tls.client_ca_file are required together with it")
		}
		creds, err := grpcutil.ServerTLS(grpcutil.ServerTLSConfig{CertFile: certFile, KeyFile: keyFile, ClientCAFile: caFile})
		if err != nil {
			logger.Fatal("unable to configure relay server TLS", zap.Error(err))
		}
		opts = append(opts, creds)
	}

	relayPort := viper.GetInt("googlehome.relay_listen_port")
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", relayPort))
	if err != nil {
		logger.Fatal("error listening", zap.Error(err), zap.Int("port", relayPort))
	}

	grpcServer := grpc.NewServer(opts...)
	relay.RegisterRelayServiceServer(grpcServer, relayAPI)

	logger.Info("serving relay requests", zap.String("address", lis.Addr().String()))
	if err := grpcServer.Serve(lis); err != nil {
		logger.Fatal("grpc server error", zap.Error(err))
	}
}
