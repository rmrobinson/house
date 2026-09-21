// policyd runs the policy execution engine's HTTP UI as a standalone
// daemon.
//
// It is not yet wired to a real house: its HomeAPI is stubHomeAPI, an
// in-memory placeholder (see stubhome.go) that logs every call instead of
// touching real device/house state via bridge gRPC clients. That real
// HomeAPI adapter is separate follow-up work (Phase C in the policy engine
// plan) — this binary exists to make the engine and its UI runnable and
// visually verifiable before that adapter lands, and shouldn't need
// changes beyond swapping stubHomeAPI out once it does.
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
	"go.uber.org/zap"
)

var (
	dbPath = flag.String("db", "policy.db", "Path to the SQLite database to use")
	addr   = flag.String("addr", "localhost:8080", "Address for the HTTP UI to listen on")
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

	logger.Warn("using stub HomeAPI: no real device/house integration yet, see stubhome.go")
	home := newStubHomeAPI(logger)

	engine := policy.NewEngine(home, registry, logger, policy.WithStore(store))
	defer engine.Close()

	policy.RegisterSystemConditionTypes(engine)
	if err := policy.LoadPersistedPolicies(engine, store); err != nil {
		logger.Fatal("unable to load persisted policies", zap.Error(err))
	}
	if err := policy.LoadDefaultSystemPolicies(engine); err != nil {
		logger.Fatal("unable to load default system policies", zap.Error(err))
	}

	server := policy.NewServer(engine, registry, logger)
	httpServer := &http.Server{
		Addr:    *addr,
		Handler: server.Handler(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
