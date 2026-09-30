// policyd runs the policy execution engine's HTTP UI as a standalone
// daemon.
//
// With --bridge-addr, its HomeAPI is bridgehome.Adapter, wired to a real
// BridgeService endpoint (a single bridge, a standalone bridgefacaded, or a
// housed process with the facade embedded — policyd doesn't distinguish).
// With no --bridge-addr, it falls back to stubHomeAPI, an in-memory
// placeholder (see stubhome.go) that logs every call instead of touching
// real device/house state, so the engine and its UI stay runnable and
// visually verifiable with no bridge configured.
//
// With --house-addr and --building-id, the schedule.sun-event/
// schedule.daylight/schedule.date-range condition types' location is
// fetched once at startup from that building's HouseService Config (lat/
// lon/tz) via policy.NewLocationHomeAPI - a building's location is static
// configuration, not live state, so there's no need to poll or hold a
// connection open the way bridgehome does for device state. --lat/--lon/
// --location-tz remain as a fallback for running with no house service
// configured (e.g. local testing); --house-addr takes precedence over them
// when both are set.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/policy"
	"github.com/rmrobinson/house/service/policy/bridgehome"
)

var (
	dbPath     = flag.String("db", "policy.db", "Path to the SQLite database to use")
	addr       = flag.String("addr", "localhost:8080", "Address for the HTTP UI to listen on")
	bridgeAddr = flag.String("bridge-addr", "", "BridgeService address to connect to (a single bridge, a bridgefacaded, or a housed with facade embedded); if empty, uses an in-memory stub with no real device/house integration")
	houseAddr  = flag.String("house-addr", "", "HouseService address to fetch --building-id's location (lat/lon/tz) from at startup, for the schedule.sun-event/schedule.daylight/schedule.date-range condition types; if empty, falls back to --lat/--lon/--location-tz")
	buildingID = flag.String("building-id", "", "Building ID to fetch location from; required if --house-addr is set")
	lat        = flag.Float64("lat", 0, "Building latitude in degrees, used if --house-addr is empty; leave both --lat and --lon at 0 to skip wrapping HomeAPI with a fixed location entirely")
	lon        = flag.Float64("lon", 0, "Building longitude in degrees; see --lat")
	locationTZ = flag.String("location-tz", "", "IANA timezone (e.g. America/Toronto), used if --house-addr is empty; defaults to the engine process's local zone")
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
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		logger.Fatal("unable to open db", zap.Error(err))
	}
	defer sqlDB.Close()

	registry := policy.NewConditionRegistry()
	store, err := policy.NewSQLiteStore(logger, sqlDB, registry)
	if err != nil {
		logger.Fatal("unable to initialize store", zap.Error(err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var home policy.HomeAPI
	var adapter *bridgehome.Adapter
	if *bridgeAddr != "" {
		adapter = bridgehome.New(logger, *bridgeAddr)
		home = adapter
	} else {
		logger.Warn("using stub HomeAPI: no real device/house integration yet, see stubhome.go")
		home = newStubHomeAPI(logger)
	}

	loc := buildingLocation{lat: *lat, lon: *lon, tz: *locationTZ}
	if *houseAddr != "" {
		if *buildingID == "" {
			logger.Fatal("--building-id is required when --house-addr is set")
		}

		houseConn, err := grpcutil.DialInsecure(*houseAddr)
		if err != nil {
			logger.Fatal("unable to dial house service", zap.String("address", *houseAddr), zap.Error(err))
		}
		defer houseConn.Close()
		houseClient := api2.NewHouseServiceClient(houseConn)

		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		loc, err = fetchBuildingLocation(fetchCtx, houseClient, *buildingID)
		cancel()
		if err != nil {
			logger.Fatal("unable to fetch building location", zap.Error(err))
		}
		logger.Info("using location from house service",
			zap.String("building_id", *buildingID), zap.Float64("lat", loc.lat), zap.Float64("lon", loc.lon), zap.String("tz", loc.tz))
	}

	if loc.lat != 0 || loc.lon != 0 || loc.tz != "" {
		home = policy.NewLocationHomeAPI(home, loc.lat, loc.lon, loc.tz)
	}

	engine := policy.NewEngine(home, registry, logger, policy.WithStore(store))
	defer engine.Close()

	policy.RegisterSystemConditionTypes(engine)
	policy.RegisterBuiltinConditionTypes(engine)
	policy.RegisterLocationConditionTypes(engine)
	if err := policy.LoadPersistedPolicies(engine, store); err != nil {
		logger.Fatal("unable to load persisted policies", zap.Error(err))
	}
	if err := policy.LoadDefaultSystemPolicies(engine); err != nil {
		logger.Fatal("unable to load default system policies", zap.Error(err))
	}

	// Started only once persisted/default policies are already registered
	// (and their conditions Start()ed and subscribed), so the stream can't
	// publish anything a condition would need to catch before it's listening.
	if adapter != nil {
		adapter.Start(ctx, engine)
	}

	server := policy.NewServer(engine, registry, logger)
	httpServer := &http.Server{
		Addr:    *addr,
		Handler: server.Handler(),
	}

	go func() {
		logger.Info("serving policy UI", zap.String("address", *addr))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("http server error", zap.Error(err))
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("error shutting down http server", zap.Error(err))
	}
}
