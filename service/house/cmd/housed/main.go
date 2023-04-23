package main

import (
	"net"

	"go.uber.org/zap"
	"google.golang.org/grpc"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	listener, err := net.Listen("tcp", "localhost:1337")
	if err != nil {
		logger.Fatal("error listening",
			zap.Error(err),
			zap.Int("port", 1337),
		)
	}
	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)
	grpcServer.Serve(listener)
}
