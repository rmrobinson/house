package main

import (
	"fmt"
	"net"
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

	svc := bridge.NewService(logger)

	eb := NewExampleBridge(logger, svc)

	go func() {
		// Use this to mimic a bridge which takes a bit of time to detect and connect
		time.Sleep(time.Second * 15)
		logger.Debug("bridge initialized")

		svc.RegisterHandler(eb, eb.b)

		svc.UpdateDevice(eb.d1.toDevice())
		svc.UpdateDevice(eb.d2.toDevice())
	}()

	go eb.Run()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", 12345))
	if err != nil {
		logger.Fatal("failed to listen", zap.Error(err))
	}

	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)

	api2.RegisterBridgeServiceServer(grpcServer, svc.API())

	logger.Info("serving requests", zap.String("address", lis.Addr().String()))
	grpcServer.Serve(lis)
}
