package main

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	charger := NewCharger(logger, "http://10.17.16.103", &http.Client{})
	cb := NewChargerBridge(logger, charger)

	b := &api2.Bridge{
		Id:               "temporary-id",
		IsReachable:      true,
		ModelId:          "TWC1",
		Manufacturer:     "Faltung Networks",
		ModelName:        nil,
		ModelDescription: nil,
		Config: &api2.Bridge_Config{
			Name:        "",
			Description: "",
			Address: &api2.Address{
				Ip: &api2.Address_Ip{
					Host: "10.17.20.102",
					Port: 12345,
				},
			},
			Timezone: time.Local.String(),
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	svc := bridge.NewService(logger, cb, b)
	api := bridge.NewAPI(logger, svc)

	cb.svc = svc
	svc.UpdateDevice(charger.deviceFromCachedState())

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", 12345))
	if err != nil {
		logger.Fatal("failed to listen", zap.Error(err))
	}

	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)

	api2.RegisterBridgeServiceServer(grpcServer, api)

	logger.Info("serving requests", zap.String("address", lis.Addr().String()))
	grpcServer.Serve(lis)
}