package main

import (
	"context"
	"fmt"
	"net"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/bridge/facade"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigName("bridgefacade")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/etc/house")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	viper.SetDefault("bridge.listen_port", 17020)

	if err := viper.ReadInConfig(); err != nil {
		logger.Fatal("unable to read config", zap.Error(err))
	}

	port := viper.GetInt("bridge.listen_port")

	cfg, err := facade.LoadConfig(logger, port)
	if err != nil {
		logger.Fatal("unable to load facade config", zap.Error(err))
	}
	if cfg == nil {
		logger.Fatal("facade.bridges config is empty; nothing to connect to")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f := facade.NewFromConfig(ctx, logger, cfg)
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		logger.Fatal("error listening", zap.Error(err), zap.Int("port", port))
	}

	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)
	api2.RegisterBridgeServiceServer(grpcServer, f)

	logger.Info("serving requests", zap.String("address", lis.Addr().String()))
	if err := grpcServer.Serve(lis); err != nil {
		logger.Fatal("grpc server stopped", zap.Error(err))
	}
}
