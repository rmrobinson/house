package bridge

import (
	"fmt"
	"net"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
)

// Server creates a new network server hosting the Bridge gRPC server.
type Server struct {
	logger     *zap.Logger
	grpcServer *grpc.Server
	svc        *Service
}

// NewServer creates a new server with an opinionated set of options set.
// Once ready it is necessary to call Serve() or ServeOnPort() to expose the service.
func NewServer(logger *zap.Logger, svc *Service) *Server {
	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)

	api2.RegisterBridgeServiceServer(grpcServer, svc.API())

	return &Server{
		logger:     logger,
		grpcServer: grpcServer,
		svc:        svc,
	}
}

// Serve runs the network listener on a random port.
func (s *Server) Serve() error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", 0))
	if err != nil {
		s.logger.Fatal("failed to listen", zap.Error(err))
	}

	s.logger.Info("accepting requests", zap.String("address", lis.Addr().String()))
	return s.grpcServer.Serve(lis)
}

// ServeOnPort runs the network listener on the specified port.
func (s *Server) ServeOnPort(port int) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		s.logger.Fatal("failed to listen", zap.Error(err))
	}

	s.logger.Info("accepting requests", zap.String("address", lis.Addr().String()))
	return s.grpcServer.Serve(lis)
}
