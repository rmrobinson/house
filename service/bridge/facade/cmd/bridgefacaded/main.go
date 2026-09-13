package main

import (
	"context"
	"fmt"
	"net"

	"github.com/google/uuid"
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

	if len(viper.GetString("bridge.id")) < 1 {
		bridgeID := uuid.New().String()

		logger.Info("config missing bridge id, saving new bridge id",
			zap.String("bridge_id", bridgeID))

		viper.Set("bridge.id", bridgeID)

		if err := viper.WriteConfig(); err != nil {
			logger.Fatal("unable to write new config", zap.Error(err))
		}
	}

	// bridge.address is the network address downstream clients use to reach
	// this facade - published as the Address of every device it proxies (see
	// facade.present()), since a client of the facade should never need to
	// dial an individual upstream bridge directly.
	selfAddress := viper.GetString("bridge.address")
	if len(selfAddress) < 1 {
		logger.Fatal("bridge.address is required: the address downstream clients use to reach this facade")
	}

	var addrs []string
	if err := viper.UnmarshalKey("facade.bridges", &addrs); err != nil {
		logger.Fatal("unable to parse facade.bridges config", zap.Error(err))
	}
	if len(addrs) < 1 {
		logger.Fatal("facade.bridges config is empty; nothing to connect to")
	}

	self := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "HouseBridgeFacade",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
		},
	}

	f := facade.New(logger, self, selfAddress)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, addr := range addrs {
		logger.Info("connecting to upstream bridge", zap.String("address", addr))
		f.Connect(ctx, addr)
	}

	port := viper.GetInt("bridge.listen_port")
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
