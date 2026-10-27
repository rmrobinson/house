// Command viewerui serves a server-rendered, read-mostly house dashboard
// (html/template + htmx, no JS framework, no grpc-web) over HouseService
// (building/floor/room structure and live room properties) and the
// BridgeService facade (device state, commands). Same architecture as
// adminui - see README.md for the route map.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/configutil"
	"github.com/rmrobinson/house/service/lib/grpcutil"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("viewerui.listen_port", 8081)

	configPath, err := configutil.FindConfigFile("viewerui", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	houseAddr := viper.GetString("viewerui.house_addr")
	if len(houseAddr) < 1 {
		logger.Fatal("viewerui.house_addr is required: address of the housed HouseService")
	}
	bridgeAddr := viper.GetString("viewerui.bridge_facade_addr")
	if len(bridgeAddr) < 1 {
		bridgeAddr = houseAddr
	}

	// Optional mutual TLS - same keys and semantics as adminui.tls.*.
	var tlsCfg *grpcutil.ClientTLSConfig
	if certFile := viper.GetString("viewerui.tls.cert_file"); len(certFile) > 0 {
		tlsCfg = &grpcutil.ClientTLSConfig{
			CertFile:   certFile,
			KeyFile:    viper.GetString("viewerui.tls.key_file"),
			CAFile:     viper.GetString("viewerui.tls.ca_file"),
			ServerName: viper.GetString("viewerui.tls.server_name"),
		}
	}

	// ctx ends on SIGINT/SIGTERM; it also parents every request context, so
	// open SSE streams end and let Shutdown finish.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := dialServer(ctx, logger, houseAddr, bridgeAddr, tlsCfg)
	if err != nil {
		logger.Fatal("unable to start viewer ui", zap.Error(err))
	}

	// Opening the garage door asks for this PIN; unset, those actions are refused.
	if s.doorPIN, err = newPINGate(viper.GetString("viewerui.door_pin_hash")); err != nil {
		logger.Fatal("invalid viewerui.door_pin_hash", zap.Error(err))
	}
	if s.doorPIN.hash == nil {
		logger.Warn("viewerui.door_pin_hash is not set: opening the garage door from the viewer is disabled")
	}

	if ice := viper.GetStringSlice("viewerui.ice_servers"); len(ice) > 0 {
		j, err := json.Marshal(ice)
		if err != nil {
			logger.Fatal("invalid viewerui.ice_servers", zap.Error(err))
		}
		s.iceServersJSON = string(j)
	}

	// Optional exterior weather: weather-server's WeatherService. Plaintext
	// gRPC by default, or one-way TLS when weather_tls is set (the nginx
	// front on :443), verified against weather_ca_file or the system roots; weather_psk matches its WEATHER_PSK if it has one.
	if weatherAddr := viper.GetString("viewerui.weather_addr"); weatherAddr != "" {
		var conn *grpc.ClientConn
		var err error
		if viper.GetBool("viewerui.weather_tls") {
			conn, err = grpcutil.DialServerTLS(weatherAddr, viper.GetString("viewerui.weather_ca_file"))
		} else {
			conn, err = grpcutil.DialInsecure(weatherAddr)
		}
		if err != nil {
			logger.Fatal("unable to dial weather server", zap.String("addr", weatherAddr), zap.Error(err))
		}
		defer conn.Close()
		s.weather = startWeatherFeed(ctx, logger, conn, viper.GetString("viewerui.weather_psk"))
	}

	// Optional device-history sparklines: metricsd's MetricsService. Uses the
	// same mTLS identity as the house dial; metrics_server_name replaces
	// tls.server_name for this connection only (that one names housed).
	if metricsAddr := viper.GetString("viewerui.metrics_addr"); metricsAddr != "" {
		var metricsTLS *grpcutil.ClientTLSConfig
		if tlsCfg != nil {
			c := *tlsCfg
			c.ServerName = viper.GetString("viewerui.metrics_server_name")
			metricsTLS = &c
		}
		conn, err := grpcutil.Dial(metricsAddr, metricsTLS)
		if err != nil {
			logger.Fatal("unable to dial metrics server", zap.String("addr", metricsAddr), zap.Error(err))
		}
		defer conn.Close()
		s.metrics = newMetricsFeed(logger, api2.NewMetricsServiceClient(conn))
	}

	port := viper.GetInt("viewerui.listen_port")
	logger.Info("serving viewer ui", zap.Int("port", port))
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: s.routes(),
		// No WriteTimeout: the SSE stream is a long-lived response.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal("http server stopped", zap.Error(err))
	}
	logger.Info("viewer ui stopped")
}
