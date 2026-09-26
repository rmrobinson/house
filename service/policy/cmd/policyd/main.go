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
// With --lat/--lon (and optionally --location-tz), whichever HomeAPI that
// resolves to is further wrapped in policy.LocationHomeAPI, so the
// schedule.sun-event/schedule.daylight/schedule.date-range condition types
// have a location to compute sunrise/sunset/calendar-date facts from - see
// LocationHomeAPI's doc comment for why this is a static stand-in rather
// than a house-service lookup.
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

	_ "github.com/mattn/go-sqlite3"
	"github.com/rmrobinson/house/service/policy"
	"github.com/rmrobinson/house/service/policy/bridgehome"
	"go.uber.org/zap"
)

var (
	dbPath     = flag.String("db", "policy.db", "Path to the SQLite database to use")
	addr       = flag.String("addr", "localhost:8080", "Address for the HTTP UI to listen on")
	bridgeAddr = flag.String("bridge-addr", "", "BridgeService address to connect to (a single bridge, a bridgefacaded, or a housed with facade embedded); if empty, uses an in-memory stub with no real device/house integration")
	lat        = flag.Float64("lat", 0, "Building latitude in degrees, for the schedule.sun-event/schedule.daylight condition types; leave both --lat and --lon at 0 to skip wrapping HomeAPI with a fixed location entirely")
	lon        = flag.Float64("lon", 0, "Building longitude in degrees; see --lat")
	locationTZ = flag.String("location-tz", "", "IANA timezone for the schedule.sun-event/schedule.daylight/schedule.date-range condition types (e.g. America/Toronto); defaults to the engine process's local zone")
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

	if *lat != 0 || *lon != 0 || *locationTZ != "" {
		home = policy.NewLocationHomeAPI(home, *lat, *lon, *locationTZ)
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
