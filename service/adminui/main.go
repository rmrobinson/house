// Command adminui serves a server-rendered admin UI (html/template + htmx,
// no JS framework, no grpc-web) over HouseService (building/floor/room
// topology, device-to-room linking) and the BridgeService facade (device
// state, live updates). See README.md for the page/route map.
package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/lib/configutil"
	"github.com/rmrobinson/house/service/lib/grpcutil"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("adminui.listen_port", 8080)

	configPath, err := configutil.FindConfigFile("adminui", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	// house_addr/bridge_facade_addr/policy_addr (endpoints, settings.go) are
	// the addresses this process dials - read once here for the first
	// generation, but from then on editable live from the Settings page,
	// which persists a change back into configPath via
	// configutil.PersistValues rather than requiring a restart.
	ep := endpoints{
		HouseAddr:        viper.GetString("adminui.house_addr"),
		BridgeFacadeAddr: viper.GetString("adminui.bridge_facade_addr"),
		PolicyAddr:       viper.GetString("adminui.policy_addr"),
	}
	if len(ep.HouseAddr) < 1 {
		logger.Fatal("adminui.house_addr is required: address of the housed HouseService")
	}

	// Optional mutual TLS - blank (the default) dials plaintext gRPC, same
	// as every other client in this repo. One cert/key for adminui as a
	// single principal talking to housed/the facade/policyd, whichever
	// addresses those end up being - not itself editable from the Settings
	// page (see endpoints' doc comment in settings.go).
	var tlsCfg *grpcutil.ClientTLSConfig
	if certFile := viper.GetString("adminui.tls.cert_file"); len(certFile) > 0 {
		tlsCfg = &grpcutil.ClientTLSConfig{
			CertFile:   certFile,
			KeyFile:    viper.GetString("adminui.tls.key_file"),
			CAFile:     viper.GetString("adminui.tls.ca_file"),
			ServerName: viper.GetString("adminui.tls.server_name"),
		}
	}

	a, err := newApp(context.Background(), logger, configPath, tlsCfg, ep)
	if err != nil {
		logger.Fatal("unable to start admin ui", zap.Error(err))
	}

	port := viper.GetInt("adminui.listen_port")
	logger.Info("serving admin ui", zap.Int("port", port))
	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), a.mux); err != nil {
		logger.Fatal("http server stopped", zap.Error(err))
	}
}
