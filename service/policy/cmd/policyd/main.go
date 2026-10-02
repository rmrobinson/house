// policyd runs the policy execution engine as a standalone daemon, exposing
// it over api2.PolicyServiceServer - it has no UI of its own; that lives in
// service/adminui, driven entirely by this gRPC API (see service/policy/
// grpc.go).
//
// With --bridge-addr, its HomeAPI is bridgehome.Adapter, wired to a real
// BridgeService endpoint (a single bridge, a standalone bridgefacaded, or a
// housed process with the facade embedded — policyd doesn't distinguish).
// With no --bridge-addr, it falls back to stubHomeAPI, an in-memory
// placeholder (see stubhome.go) that logs every call instead of touching
// real device/house state, so the engine stays runnable with no bridge
// configured.
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
//
// The same --house-addr/--building-id connection also backs GetHouseState's
// "occupied"/"mode" keys and SetHouseState's "mode" key (see
// housestate.Adapter): Building.State has no update stream yet, so the
// adapter polls GetBuilding on an interval instead, which is what drives the
// "sys.occupied" condition type and the "sys.occupancy" default policy built
// on it. Left unconfigured (no --house-addr), both keys stay
// policy.ErrNotImplemented, same as any other HomeAPI method bridgehome/the
// stub don't back.
//
// --tls-cert/--tls-key/--tls-ca configure mutual TLS for every role this
// process plays: dialing --bridge-addr/--house-addr as a client, and --addr's
// own PolicyService listener as a server (see the flags' doc comments).
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/policy"
	"github.com/rmrobinson/house/service/policy/bridgehome"
	"github.com/rmrobinson/house/service/policy/housestate"
)

var (
	dbPath     = flag.String("db", "policy.db", "Path to the SQLite database to use")
	addr       = flag.String("addr", "localhost:8080", "Address for the PolicyService gRPC API to listen on")
	bridgeAddr = flag.String("bridge-addr", "", "BridgeService address to connect to (a single bridge, a bridgefacaded, or a housed with facade embedded); if empty, uses an in-memory stub with no real device/house integration")
	houseAddr  = flag.String("house-addr", "", "HouseService address to fetch --building-id's location (lat/lon/tz) from at startup, for the schedule.sun-event/schedule.daylight/schedule.date-range condition types; if empty, falls back to --lat/--lon/--location-tz")
	buildingID = flag.String("building-id", "", "Building ID to fetch location from; required if --house-addr is set")
	lat        = flag.Float64("lat", 0, "Building latitude in degrees, used if --house-addr is empty; leave both --lat and --lon at 0 to skip wrapping HomeAPI with a fixed location entirely")
	lon        = flag.Float64("lon", 0, "Building longitude in degrees; see --lat")
	locationTZ = flag.String("location-tz", "", "IANA timezone (e.g. America/Toronto), used if --house-addr is empty; defaults to the engine process's local zone")

	// Optional mutual TLS, one identity for every role this process plays -
	// same three files serve as this process's client certificate when
	// dialing --bridge-addr/--house-addr *and* as its server certificate for
	// --addr's own PolicyService listener (adminui, or any other
	// StreamEvents/PolicyService caller, dials back in using the same CA).
	// All three of tlsCertFile/tlsKeyFile/tlsCAFile are required together, or
	// all left blank to stay on plaintext gRPC (the default) for both roles -
	// matching bridgecli's own --tls-cert/--tls-key/--tls-ca convention, and
	// housed's house.tls.* reused for both its server and self-dial client
	// roles.
	tlsCertFile   = flag.String("tls-cert", "", "certificate file for mutual TLS: this process's client identity when dialing --bridge-addr/--house-addr, and its server identity on --addr")
	tlsKeyFile    = flag.String("tls-key", "", "key file for mutual TLS")
	tlsCAFile     = flag.String("tls-ca", "", "CA file trusted to verify --bridge-addr/--house-addr's certificate, and to verify callers connecting to --addr")
	tlsServerName = flag.String("tls-server-name", "", "hostname to verify the server's certificate against, if different from --bridge-addr/--house-addr")
)

// clientTLSConfig builds a *grpcutil.ClientTLSConfig from the --tls-* flags,
// or nil for plaintext gRPC if none are set.
func clientTLSConfig(logger *zap.Logger) *grpcutil.ClientTLSConfig {
	set := 0
	for _, f := range []string{*tlsCertFile, *tlsKeyFile, *tlsCAFile} {
		if f != "" {
			set++
		}
	}
	switch set {
	case 0:
		return nil
	case 3:
		return &grpcutil.ClientTLSConfig{CertFile: *tlsCertFile, KeyFile: *tlsKeyFile, CAFile: *tlsCAFile, ServerName: *tlsServerName}
	default:
		logger.Fatal("--tls-cert, --tls-key, and --tls-ca are required together - only some of them were set")
		return nil
	}
}

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

	tlsCfg := clientTLSConfig(logger)

	var home policy.HomeAPI
	var adapter *bridgehome.Adapter
	if *bridgeAddr != "" {
		adapter = bridgehome.New(logger, *bridgeAddr, tlsCfg)
		home = adapter
	} else {
		logger.Warn("using stub HomeAPI: no real device/house integration yet, see stubhome.go")
		home = newStubHomeAPI(logger)
	}

	loc := buildingLocation{lat: *lat, lon: *lon, tz: *locationTZ}
	// haveLocation tracks whether a location was actually configured, rather
	// than re-deriving it from loc's fields once --house-addr is in play: a
	// real building fetched from HouseService can legitimately have an unset
	// Config (lat=0, lon=0, tz="", the proto zero value), which must still
	// count as "configured" - the fetch itself is the caller's explicit
	// intent to use location-based conditions - not be silently
	// indistinguishable from "no --lat/--lon/--location-tz and no
	// --house-addr at all".
	haveLocation := *lat != 0 || *lon != 0 || *locationTZ != ""
	var houseStateAdapter *housestate.Adapter
	if *houseAddr != "" {
		if *buildingID == "" {
			logger.Fatal("--building-id is required when --house-addr is set")
		}

		// Kept open for the process lifetime (not closed after the location
		// fetch below): houseStateAdapter polls this same connection for
		// Building.State ("occupied"/"mode") and calls SetHouseMode on it for
		// the whole time policyd runs, unlike the one-shot location fetch.
		houseConn, err := grpcutil.Dial(*houseAddr, tlsCfg)
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
		haveLocation = true

		if loc.lat == 0 && loc.lon == 0 && loc.tz == "" {
			logger.Warn("building has no location configured in house service; schedule.sun-event/daylight/date-range conditions will use lat=0,lon=0 and the local timezone",
				zap.String("building_id", *buildingID))
		}
		logger.Info("using location from house service",
			zap.String("building_id", *buildingID), zap.Float64("lat", loc.lat), zap.Float64("lon", loc.lon), zap.String("tz", loc.tz))

		houseStateAdapter = housestate.New(logger, houseClient, *buildingID, home)
		home = houseStateAdapter
	}

	if haveLocation {
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
	if houseStateAdapter != nil {
		houseStateAdapter.Start(ctx, engine)
	}

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Fatal("error listening", zap.Error(err), zap.String("address", *addr))
	}

	var serverOpts []grpc.ServerOption
	if tlsCfg != nil {
		// Reuses the same cert/key/ca as the client role above - see the
		// tls-cert flag's doc comment for why one identity covers both.
		tlsOpt, err := grpcutil.ServerTLS(grpcutil.ServerTLSConfig{
			CertFile:     tlsCfg.CertFile,
			KeyFile:      tlsCfg.KeyFile,
			ClientCAFile: tlsCfg.CAFile,
		})
		if err != nil {
			logger.Fatal("unable to configure server TLS", zap.Error(err))
		}
		serverOpts = append(serverOpts, tlsOpt)
	}

	grpcServer := grpc.NewServer(serverOpts...)
	api2.RegisterPolicyServiceServer(grpcServer, policy.NewService(logger, engine, registry))

	go func() {
		logger.Info("serving policy API", zap.String("address", *addr))
		if err := grpcServer.Serve(lis); err != nil {
			logger.Fatal("grpc server error", zap.Error(err))
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	// GracefulStop waits for every in-flight RPC to finish, including a
	// StreamEvents call (adminui's policyHub holds one open indefinitely by
	// design - see service/adminui/policy_hub.go) that only ends once its
	// own ctx is cancelled or the connection drops - neither of which
	// GracefulStop itself triggers. Bounded the same way the old HTTP
	// server's Shutdown(10s) was, so a connected adminui (or any other
	// long-lived StreamEvents client) can no longer wedge shutdown
	// indefinitely; Stop forcibly cuts any RPC still running past the
	// deadline.
	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		logger.Warn("graceful shutdown timed out; forcing stop")
		grpcServer.Stop()
	}
}
