package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/configutil"
	"github.com/rmrobinson/house/grpcutil"
	"github.com/rmrobinson/house/service/bridge/facade"
	"github.com/rmrobinson/house/service/house"
	"github.com/rmrobinson/house/service/house/db"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("house.listen_port", 1337)

	configPath, err := configutil.FindConfigFile("housed", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	dbPath := viper.GetString("house.db")
	if len(dbPath) < 1 {
		logger.Fatal("house.db is required: path to the database to use")
	}

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		logger.Debug("database file missing; creating", zap.String("db_path", dbPath))
		dbFile, err := os.Create(dbPath)
		if err != nil {
			logger.Fatal("unable to create database file", zap.Error(err))
		}
		dbFile.Close()
	}

	dsn := fmt.Sprintf("file:%s?parseTime=true", dbPath)
	sqlDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		logger.Fatal("unable to open db", zap.Error(err))
	} else if sqlDB == nil {
		logger.Fatal("empty database")
	}
	defer sqlDB.Close()

	buildingDB, err := db.NewDatabase(logger, sqlDB)
	if err != nil {
		logger.Fatal("unable to initialize db", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listenPort := viper.GetInt("house.listen_port")

	// Listening before loading the facade config (rather than after) means
	// facade.LoadConfig gets the port actually bound by the OS, not just the
	// port that was asked for - the two only differ if house.listen_port is
	// ever set to 0 for an OS-assigned ephemeral port, but deriving it from
	// the real listener means that case can't silently advertise the wrong
	// port instead of failing loudly.
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", listenPort))
	if err != nil {
		logger.Fatal("error listening", zap.Error(err), zap.Int("port", listenPort))
	}
	boundPort := lis.Addr().(*net.TCPAddr).Port

	// tlsCfg, when configured, secures both this listener (HouseService, and
	// BridgeService if a facade is embedded below - they share one
	// grpc.Server) and every outbound dial housed itself makes (to its own
	// embedded facade over loopback, or to an external bridgefacaded) - one
	// principal ("housed") presenting the same identity either way.
	var tlsCfg *grpcutil.ServerTLSConfig
	var clientTLS *grpcutil.ClientTLSConfig
	var opts []grpc.ServerOption
	if certFile := viper.GetString("house.tls.cert_file"); len(certFile) > 0 {
		keyFile := viper.GetString("house.tls.key_file")
		caFile := viper.GetString("house.tls.client_ca_file")
		if len(keyFile) < 1 || len(caFile) < 1 {
			logger.Fatal("house.tls.cert_file is set; house.tls.key_file and house.tls.client_ca_file are required together with it")
		}

		tlsCfg = &grpcutil.ServerTLSConfig{CertFile: certFile, KeyFile: keyFile, ClientCAFile: caFile}
		creds, err := grpcutil.ServerTLS(*tlsCfg)
		if err != nil {
			logger.Fatal("unable to configure server TLS", zap.Error(err))
		}
		opts = append(opts, creds)

		clientTLS = &grpcutil.ClientTLSConfig{CertFile: certFile, KeyFile: keyFile, CAFile: caFile}
	}
	grpcServer := grpc.NewServer(opts...)

	// A BridgeService facade is optional, and reached one of two ways:
	//   - Embedded: if facade.bridges is configured (same shape as
	//     bridgefacaded's own config - see
	//     service/bridge/facade/cmd/bridgefacaded/bridgefacade.example.yaml),
	//     housed aggregates those upstream bridges itself and registers
	//     BridgeService on this same listener, alongside HouseService - one
	//     less process to deploy/orchestrate.
	//   - External: otherwise, if house.bridge_facade_addr is set, housed
	//     dials a separately-running bridgefacaded instead.
	// If neither is configured, linked devices are still returned but only
	// as ID-only stubs (see house.Service.resolveDevices).
	var bridgeClient api2.BridgeServiceClient
	facadeCfg, err := facade.LoadConfig(logger, boundPort, configPath)
	if err != nil {
		logger.Fatal("unable to load facade config", zap.Error(err))
	}
	switch {
	case facadeCfg != nil:
		f := facade.NewFromConfig(ctx, logger, facadeCfg)
		api2.RegisterBridgeServiceServer(grpcServer, f)

		// Dialing "localhost" needs its own TLS config: grpc verifies the
		// peer's certificate against the dial target's hostname by default,
		// which would be "localhost" here - not the hostname housed's own
		// certificate was actually issued for (e.g. "housed.myhost.house.
		// internal", per cert-agent's SAN convention). house.tls.server_name
		// overrides that check to the name the cert really carries.
		selfAddr := fmt.Sprintf("localhost:%d", boundPort)
		selfTLS := clientTLS
		if selfTLS != nil {
			serverName := viper.GetString("house.tls.server_name")
			if len(serverName) < 1 {
				logger.Fatal("house.tls.server_name is required when house.tls.* and an embedded facade are both configured - it must match the hostname housed's own certificate was issued for, since dialing \"localhost\" would otherwise fail certificate verification")
			}
			override := *selfTLS
			override.ServerName = serverName
			selfTLS = &override
		}
		conn, err := grpcutil.Dial(selfAddr, selfTLS)
		if err != nil {
			logger.Fatal("unable to dial embedded bridge facade", zap.String("address", selfAddr), zap.Error(err))
		}
		bridgeClient = api2.NewBridgeServiceClient(conn)
	case len(viper.GetString("house.bridge_facade_addr")) > 0:
		addr := viper.GetString("house.bridge_facade_addr")
		conn, err := grpcutil.Dial(addr, clientTLS)
		if err != nil {
			logger.Fatal("unable to dial bridge facade", zap.String("address", addr), zap.Error(err))
		}
		bridgeClient = api2.NewBridgeServiceClient(conn)
	default:
		logger.Warn("no bridge facade configured; linked devices will only be returned as ID-only stubs")
	}

	svc := house.NewService(logger, buildingDB, bridgeClient)
	api2.RegisterHouseServiceServer(grpcServer, svc)

	logger.Info("serving requests", zap.String("address", lis.Addr().String()))
	grpcServer.Serve(lis)
}
