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
//
// googlehome.public_base_url is this process's own publicly-reachable
// origin (e.g. "https://googlehome.example.com", matching whatever Caddy
// terminates above it) - used to build each camera session's
// cameraStreamSignalingUrl (see relay.CameraSignalingPath and
// service/googlehome/relay/session_manager.go for the Camera/WebRTC
// session this feeds).
//
// googlehome.oauth.* configures the account-linking OAuth2 front door
// (see oauth.go) - client_id/client_secret/redirect_uri here must exactly
// match what's pasted into the Actions console's "OAuth Client
// Information" section (redirect_uri is Google's own
// oauth-redirect[-sandbox].googleusercontent.com/r/<project-id> URL, not
// something this deployment picks).
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
	publicBaseURL := viper.GetString("googlehome.public_base_url")
	oauthClientID := viper.GetString("googlehome.oauth.client_id")
	oauthClientSecret := viper.GetString("googlehome.oauth.client_secret")
	oauthRedirectURI := viper.GetString("googlehome.oauth.redirect_uri")
	if agentUserID == "" || authToken == "" || credsFile == "" || publicBaseURL == "" ||
		oauthClientID == "" || oauthClientSecret == "" || oauthRedirectURI == "" {
		logger.Fatal("googlehome.agent_user_id, googlehome.auth_token, googlehome.homegraph.creds_file, googlehome.public_base_url, and googlehome.oauth.client_id/client_secret/redirect_uri are all required")
	}
	oauthCfg := oauthConfig{clientID: oauthClientID, clientSecret: oauthClientSecret, redirectURI: oauthRedirectURI, token: authToken}

	ctx := context.Background()
	hgService, err := homegraph.NewService(ctx, option.WithCredentialsFile(credsFile))
	if err != nil {
		logger.Fatal("unable to initialize homegraph client", zap.Error(err))
	}

	validator := &staticTokenValidator{token: authToken, agentUserID: agentUserID}
	providerSvc := relay.NewProviderService(logger)
	actionSvc := action.NewService(logger, validator, providerSvc, hgService)

	sessionMgr := relay.NewSessionManager()
	webrtcMgr := relay.NewWebRTCSessionManager(logger, sessionMgr, func(sessionID string) {
		// Called from whichever trigger (end/ICE failure/idle timeout)
		// tore the PeerConnection down first - expire the token record and
		// best-effort tell the owning agent to stop pulling WHEP.
		if agentID, ok := sessionMgr.Expire(sessionID); ok {
			providerSvc.StopMediaSession(agentID, sessionID)
		}
	})
	relayAPI := relay.NewAPI(logger, providerSvc, actionSvc, sessionMgr, webrtcMgr, publicBaseURL)

	authCodes := newAuthCodeStore()

	mux := http.NewServeMux()
	mux.HandleFunc(action.GoogleFulfillmentPath, actionSvc.GoogleFulfillmentHandler)
	mux.HandleFunc(relay.CameraSignalingPath, newCameraSignalingHandler(logger, sessionMgr, webrtcMgr))
	mux.HandleFunc("/oauth/authorize", newOAuthAuthorizeHandler(logger, oauthCfg, authCodes))
	mux.HandleFunc("/oauth/token", newOAuthTokenHandler(logger, oauthCfg, authCodes))

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
