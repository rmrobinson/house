package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
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

	viper.SetConfigName("housed")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/etc/house")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	viper.SetDefault("house.listen_port", 1337)

	if err := viper.ReadInConfig(); err != nil {
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

	var opts []grpc.ServerOption
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
	facadeCfg, err := facade.LoadConfig(logger)
	if err != nil {
		logger.Fatal("unable to load facade config", zap.Error(err))
	}
	switch {
	case facadeCfg != nil:
		f := facade.NewFromConfig(ctx, logger, facadeCfg)
		api2.RegisterBridgeServiceServer(grpcServer, f)

		selfAddr := fmt.Sprintf("localhost:%d", listenPort)
		conn, err := grpcutil.DialInsecure(selfAddr)
		if err != nil {
			logger.Fatal("unable to dial embedded bridge facade", zap.String("address", selfAddr), zap.Error(err))
		}
		bridgeClient = api2.NewBridgeServiceClient(conn)
	case len(viper.GetString("house.bridge_facade_addr")) > 0:
		addr := viper.GetString("house.bridge_facade_addr")
		conn, err := grpcutil.DialInsecure(addr)
		if err != nil {
			logger.Fatal("unable to dial bridge facade", zap.String("address", addr), zap.Error(err))
		}
		bridgeClient = api2.NewBridgeServiceClient(conn)
	default:
		logger.Warn("no bridge facade configured; linked devices will only be returned as ID-only stubs")
	}

	svc := house.NewService(logger, buildingDB, bridgeClient)
	api2.RegisterHouseServiceServer(grpcServer, svc)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", listenPort))
	if err != nil {
		logger.Fatal("error listening", zap.Error(err), zap.Int("port", listenPort))
	}

	logger.Info("serving requests", zap.String("address", lis.Addr().String()))
	grpcServer.Serve(lis)
}
