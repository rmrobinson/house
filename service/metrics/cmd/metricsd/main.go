// metricsd records device state changes and commands from housed's
// BridgeService update stream into InfluxDB v2. It is the only process that
// talks to Influx; house itself stays storage-free. See service/metrics for
// what is written and metricsd.example.yaml for the config shape.
//
// Everything is read-only against house: it consumes StreamUpdates and
// ListBuildings/ListRooms and issues no commands.
package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/bridgeconn"
	"github.com/rmrobinson/house/service/lib/configutil"
	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/metrics"
)

// roomsReadyTimeout bounds how long startup waits for the first room load
// before recording without room labels.
const roomsReadyTimeout = 30 * time.Second

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("metricsd.room_refresh_interval", "5m")
	viper.SetDefault("metricsd.listen_port", 17050)
	viper.SetDefault("metricsd.influx.rollup_bucket", metrics.DefaultRollupBucket)
	viper.SetDefault("metricsd.influx.flush_interval", "5s")
	viper.SetDefault("metricsd.influx.max_buffered", 100000)
	viper.SetDefault("metricsd.backfill.enabled", true)
	viper.SetDefault("metricsd.backfill.interval", metrics.DefaultBackfillInterval.String())
	viper.SetDefault("metricsd.backfill.initial_delay", metrics.DefaultBackfillInitialDelay.String())
	viper.SetDefault("metricsd.backfill.timeout", metrics.DefaultBackfillTimeout.String())

	configPath, err := configutil.FindConfigFile("metricsd", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	houseAddr := viper.GetString("metricsd.house_addr")
	if len(houseAddr) < 1 {
		logger.Fatal("metricsd.house_addr is required: address of the housed BridgeService/HouseService")
	}

	// Optional mutual TLS - same keys and semantics as viewerui.tls.*.
	var tlsCfg *grpcutil.ClientTLSConfig
	if certFile := viper.GetString("metricsd.tls.cert_file"); len(certFile) > 0 {
		tlsCfg = &grpcutil.ClientTLSConfig{
			CertFile:   certFile,
			KeyFile:    viper.GetString("metricsd.tls.key_file"),
			CAFile:     viper.GetString("metricsd.tls.ca_file"),
			ServerName: viper.GetString("metricsd.tls.server_name"),
		}
	}

	influxCfg := metrics.InfluxConfig{
		URL:           viper.GetString("metricsd.influx.url"),
		Org:           viper.GetString("metricsd.influx.org"),
		Bucket:        viper.GetString("metricsd.influx.bucket"),
		Token:         viper.GetString("metricsd.influx.token"),
		FlushInterval: viper.GetDuration("metricsd.influx.flush_interval"),
		MaxBuffered:   viper.GetInt("metricsd.influx.max_buffered"),
	}
	writer, err := metrics.NewInfluxWriter(logger, influxCfg)
	if err != nil {
		logger.Fatal("invalid influx config", zap.Error(err))
	}

	sampling := samplingConfig()
	rooms := metrics.NewRoomResolver(logger)
	svc := metrics.NewService(logger, writer, metrics.NewSampler(sampling), rooms)

	// Each list overrides its default only when set, so "details: []" turns
	// the default network-client exclusion off.
	excludes := metrics.DefaultExcludes()
	if viper.IsSet("metricsd.exclude.details") {
		excludes.Details = viper.GetStringSlice("metricsd.exclude.details")
	}
	excludes.BridgeIDs = viper.GetStringSlice("metricsd.exclude.bridge_ids")
	excludes.DeviceIDs = viper.GetStringSlice("metricsd.exclude.device_ids")
	svc.SetExcludes(excludes)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// writerDone lets shutdown wait for the writer's final flush; exiting as
	// soon as the stream loop returns would cut it off.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		writer.Run(ctx)
	}()

	// Dialed inside a retry loop, not fatally at startup: with TLS the dial
	// reads the cert files immediately, and a freshly deployed service can
	// start before cert-agent has issued its cert. Failing here would
	// crash-loop the container instead of just waiting for the cert.
	go bridgeconn.Retry(ctx, bridgeconn.NewBackoff(),
		func(err error) {
			logger.Warn("unable to reach house for room links, will retry", zap.String("addr", houseAddr), zap.Error(err))
		},
		func(ctx context.Context, backoff *bridgeconn.Backoff) error {
			houseConn, err := grpcutil.Dial(houseAddr, tlsCfg)
			if err != nil {
				return err
			}
			defer houseConn.Close()
			backoff.Reset()
			rooms.Run(ctx, api2.NewHouseServiceClient(houseConn), viper.GetDuration("metricsd.room_refresh_interval"))
			return nil
		})

	if err := serveSeries(ctx, logger, influxCfg); err != nil {
		logger.Fatal("unable to serve metrics API", zap.Error(err))
	}

	logger.Info("recording house metrics",
		zap.String("house_addr", houseAddr),
		zap.String("sampling_mode", string(sampling.Mode)),
		zap.Duration("heartbeat", sampling.Heartbeat))

	// The stream's initial replay and the backfill both stamp each point with
	// a device label that includes its room, so hold them until the rooms have
	// loaded. Otherwise every restart writes a burst of room-less labels that
	// charts as a duplicate of each device. If house is down, record anyway.
	if !rooms.WaitReady(ctx, roomsReadyTimeout) && ctx.Err() == nil {
		logger.Warn("rooms not loaded yet; recording with room-less device labels until they are",
			zap.Duration("waited", roomsReadyTimeout))
	}

	conn := bridgeconn.New(logger, houseAddr, tlsCfg)

	if viper.GetBool("metricsd.backfill.enabled") {
		querier, err := metrics.NewInfluxQuerier(influxCfg)
		if err != nil {
			logger.Fatal("invalid influx config", zap.Error(err))
		}
		backfiller := metrics.NewBackfiller(logger, metrics.BackfillConfig{
			Interval:     viper.GetDuration("metricsd.backfill.interval"),
			InitialDelay: viper.GetDuration("metricsd.backfill.initial_delay"),
			Timeout:      viper.GetDuration("metricsd.backfill.timeout"),
		}, svc, writer, querier, conn.Client)
		go backfiller.Run(ctx)
		logger.Info("history backfill enabled", zap.Duration("interval", viper.GetDuration("metricsd.backfill.interval")))
	}

	conn.Run(ctx, svc.HandleUpdate, nil)
	<-writerDone

	logger.Info("shutting down",
		zap.Int64("points_written", writer.Written()),
		zap.Int64("points_dropped", writer.Dropped()))
}

// serveSeries starts the MetricsService gRPC server in the background and
// stops it when ctx ends. Reads use metricsd.influx.read_token when set (it
// must be able to read both the raw and rollup buckets); otherwise the write
// token is tried, which only reaches the raw bucket.
func serveSeries(ctx context.Context, logger *zap.Logger, influxCfg metrics.InfluxConfig) error {
	readCfg := influxCfg
	readCfg.RollupBucket = viper.GetString("metricsd.influx.rollup_bucket")
	if t := viper.GetString("metricsd.influx.read_token"); t != "" {
		readCfg.Token = t
	}
	querier, err := metrics.NewInfluxQuerier(readCfg)
	if err != nil {
		return err
	}

	// Serving TLS reuses metricsd.tls.*: the same cert authenticates the
	// dial to house and this listener. Clients are verified against
	// client_ca_file, falling back to ca_file (one CA signs every service).
	// Without TLS the API is unauthenticated, so it is only reachable from
	// this host.
	host := "127.0.0.1"
	var opts []grpc.ServerOption
	if certFile := viper.GetString("metricsd.tls.cert_file"); certFile != "" {
		host = ""
		caFile := viper.GetString("metricsd.tls.client_ca_file")
		if caFile == "" {
			caFile = viper.GetString("metricsd.tls.ca_file")
		}
		if caFile == "" {
			return fmt.Errorf("metricsd.tls.cert_file is set; metricsd.tls.client_ca_file (or ca_file) is required to verify clients")
		}
		creds, err := grpcutil.ServerTLS(grpcutil.ServerTLSConfig{
			CertFile:     certFile,
			KeyFile:      viper.GetString("metricsd.tls.key_file"),
			ClientCAFile: caFile,
		})
		if err != nil {
			return fmt.Errorf("configure server TLS: %w", err)
		}
		opts = append(opts, creds)
	}

	port := viper.GetInt("metricsd.listen_port")
	if host != "" {
		logger.Warn("metricsd.tls.cert_file is unset; serving the metrics API unauthenticated on loopback only",
			zap.Int("port", port))
	}
	lis, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", port, err)
	}

	srv := grpc.NewServer(opts...)
	api2.RegisterMetricsServiceServer(srv, metrics.NewServer(logger, querier))
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	go func() {
		logger.Info("serving metrics API", zap.String("address", lis.Addr().String()))
		if err := srv.Serve(lis); err != nil {
			logger.Error("metrics API server error", zap.Error(err))
		}
	}()
	return nil
}

// samplingConfig layers metricsd.sampling.* over the plan's defaults, so a
// config that sets only one deadband keeps all the others.
func samplingConfig() metrics.SamplingConfig {
	cfg := metrics.DefaultSampling()

	if mode := viper.GetString("metricsd.sampling.mode"); mode != "" {
		cfg.Mode = metrics.SamplingMode(mode)
	}
	if viper.IsSet("metricsd.sampling.heartbeat") {
		cfg.Heartbeat = viper.GetDuration("metricsd.sampling.heartbeat")
	}
	cfg.DefaultDeadband = viper.GetFloat64("metricsd.sampling.default_deadband")
	flattenDeadbands("", viper.GetStringMap("metricsd.sampling.deadband"), cfg.Deadband)
	return cfg
}

// flattenDeadbands copies m into out, joining nested keys with ".". viper
// treats "." as a nesting delimiter, so a "temperature.value" key written in
// YAML arrives as {temperature: {value: 0.1}}.
func flattenDeadbands(prefix string, m map[string]interface{}, out map[string]float64) {
	for k, v := range m {
		key := prefix + k
		switch n := v.(type) {
		case map[string]interface{}:
			flattenDeadbands(key+".", n, out)
		case float64:
			out[key] = n
		case int:
			out[key] = float64(n)
		case int64:
			out[key] = float64(n)
		}
	}
}
