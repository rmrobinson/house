package main

import (
	"database/sql"
	"flag"
	"fmt"
	"net"
	"os"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/grpcutil"
	"github.com/rmrobinson/house/service/house"
	"github.com/rmrobinson/house/service/house/db"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

var (
	dbPath = flag.String("db", "", "Path to the database to use")
	// bridgeFacadeAddr is the BridgeService facade's address, used to
	// resolve full Device data for devices linked to a room (see
	// house.Service.resolveDevice). If unset, linked devices are still
	// returned but only as ID-only stubs.
	bridgeFacadeAddr = flag.String("bridge_facade_addr", "", "Address of the BridgeService facade")
)

func main() {
	flag.Parse()

	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	if _, err := os.Stat(*dbPath); os.IsNotExist(err) {
		logger.Debug("database file missing; creating", zap.String("db_path", *dbPath))
		dbFile, err := os.Create(*dbPath)
		if err != nil {
			logger.Fatal("unable to create database file", zap.Error(err))
		}
		dbFile.Close()
	}

	dsn := fmt.Sprintf("file:%s?parseTime=true", *dbPath)
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

	var bridgeClient api2.BridgeServiceClient
	if len(*bridgeFacadeAddr) > 0 {
		conn, err := grpcutil.DialInsecure(*bridgeFacadeAddr)
		if err != nil {
			logger.Fatal("unable to dial bridge facade", zap.String("address", *bridgeFacadeAddr), zap.Error(err))
		}
		bridgeClient = api2.NewBridgeServiceClient(conn)
	} else {
		logger.Warn("bridge_facade_addr not set; linked devices will only be returned as ID-only stubs")
	}

	svc := house.NewService(logger, buildingDB, bridgeClient)

	lis, err := net.Listen("tcp", "localhost:1337")
	if err != nil {
		logger.Fatal("error listening",
			zap.Error(err),
			zap.Int("port", 1337),
		)
	}

	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)

	api2.RegisterHouseServiceServer(grpcServer, svc)

	logger.Info("serving requests", zap.String("address", lis.Addr().String()))
	grpcServer.Serve(lis)
}
