package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/spf13/viper"

	"github.com/rmrobinson/house/grpcutil"
	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigName("roku")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/etc/house")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	viper.SetDefault("bridge.refresh_interval", 300)
	viper.SetDefault("bridge.listen_port", 17005)

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

	rb := NewRokuBridge(logger, svc)

	// Once we've successfully gotten the device state, register the handler and device with the service
	svc.RegisterHandler(rb, rb.b)

	// Check for updates periodically
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rb.Run(ctx)

	var tlsCfg *grpcutil.ServerTLSConfig
	if certFile := viper.GetString("bridge.tls.cert_file"); len(certFile) > 0 {
		tlsCfg = &grpcutil.ServerTLSConfig{
			CertFile:     certFile,
			KeyFile:      viper.GetString("bridge.tls.key_file"),
			ClientCAFile: viper.GetString("bridge.tls.client_ca_file"),
		}
	}

	s, err := bridge.NewServer(logger, svc, tlsCfg)
	if err != nil {
		logger.Fatal("unable to create bridge server", zap.Error(err))
	}
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
