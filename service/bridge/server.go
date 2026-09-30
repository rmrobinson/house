package bridge

import (
	"fmt"
	"net"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/grpcutil"
)

// TLSConfigFromViper reads bridge.tls.{cert,key,client_ca}_file from viper
// (the config already loaded by the caller's main.go) and returns a
// *grpcutil.ServerTLSConfig, or nil if bridge.tls.cert_file is unset -
// TLS stays opt-in per bridge. Centralized here so every bridge main.go
// reads this the same way instead of repeating the same three GetString
// calls and nil check.
//
// Returns an error if bridge.tls.cert_file is set but bridge.tls.key_file
// or bridge.tls.client_ca_file is left blank, so a half-configured TLS
// setup fails with a clear config error here rather than a confusing
// low-level file error deep inside grpcutil.ServerTLS/loadCAPool (e.g.
// "reading : no such file or directory").
func TLSConfigFromViper() (*grpcutil.ServerTLSConfig, error) {
	certFile := viper.GetString("bridge.tls.cert_file")
	if len(certFile) < 1 {
		return nil, nil
	}
	keyFile := viper.GetString("bridge.tls.key_file")
	clientCAFile := viper.GetString("bridge.tls.client_ca_file")
	if len(keyFile) < 1 || len(clientCAFile) < 1 {
		return nil, fmt.Errorf("bridge.tls.cert_file is set; bridge.tls.key_file and bridge.tls.client_ca_file are required together with it")
	}
	return &grpcutil.ServerTLSConfig{
		CertFile:     certFile,
		KeyFile:      keyFile,
		ClientCAFile: clientCAFile,
	}, nil
}

// Server creates a new network server hosting the Bridge gRPC server.
type Server struct {
	logger     *zap.Logger
	grpcServer *grpc.Server
	svc        *Service
}

// NewServer creates a new server with an opinionated set of options set.
// tlsCfg is optional - pass nil to serve plaintext gRPC (the historical
// default), or a populated *grpcutil.ServerTLSConfig to require mutual TLS.
// Once ready it is necessary to call Serve() or ServeOnPort() to expose the service.
func NewServer(logger *zap.Logger, svc *Service, tlsCfg *grpcutil.ServerTLSConfig) (*Server, error) {
	var opts []grpc.ServerOption
	if tlsCfg != nil {
		creds, err := grpcutil.ServerTLS(*tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("configuring server TLS: %w", err)
		}
		opts = append(opts, creds)
	}
	grpcServer := grpc.NewServer(opts...)

	api2.RegisterBridgeServiceServer(grpcServer, svc.API())

	return &Server{
		logger:     logger,
		grpcServer: grpcServer,
		svc:        svc,
	}, nil
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
