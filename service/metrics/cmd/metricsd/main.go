// metricsd records device state changes and commands from housed's
// BridgeService update stream into InfluxDB v2. It is the only process that
// talks to Influx; house itself stays storage-free. See service/metrics for
// what is written and metricsd.example.yaml for the config shape.
//
// Everything is read-only against house: it consumes StreamUpdates and
// ListDeviceLinks and issues no commands.
package main

import (
	"bytes"
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/bridgeconn"
	"github.com/rmrobinson/house/service/lib/configutil"
	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/metrics"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("metricsd.room_refresh_interval", "5m")
	viper.SetDefault("metricsd.influx.flush_interval", "5s")
	viper.SetDefault("metricsd.influx.max_buffered", 100000)

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

	writer, err := metrics.NewInfluxWriter(logger, metrics.InfluxConfig{
		URL:           viper.GetString("metricsd.influx.url"),
		Org:           viper.GetString("metricsd.influx.org"),
		Bucket:        viper.GetString("metricsd.influx.bucket"),
		Token:         viper.GetString("metricsd.influx.token"),
		FlushInterval: viper.GetDuration("metricsd.influx.flush_interval"),
		MaxBuffered:   viper.GetInt("metricsd.influx.max_buffered"),
	})
	if err != nil {
		logger.Fatal("invalid influx config", zap.Error(err))
	}

	sampling := samplingConfig()
	rooms := metrics.NewRoomResolver(logger)
	svc := metrics.NewService(logger, writer, metrics.NewSampler(sampling), rooms)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// writerDone lets shutdown wait for the writer's final flush; exiting as
	// soon as the stream loop returns would cut it off.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		writer.Run(ctx)
	}()

	houseConn, err := grpcutil.Dial(houseAddr, tlsCfg)
	if err != nil {
		logger.Fatal("unable to dial house", zap.Error(err), zap.String("addr", houseAddr))
	}
	defer houseConn.Close()
	go rooms.Run(ctx, api2.NewHouseServiceClient(houseConn), viper.GetDuration("metricsd.room_refresh_interval"))

	logger.Info("recording house metrics",
		zap.String("house_addr", houseAddr),
		zap.String("sampling_mode", string(sampling.Mode)),
		zap.Duration("heartbeat", sampling.Heartbeat))

	conn := bridgeconn.New(logger, houseAddr, tlsCfg)
	conn.Run(ctx, svc.HandleUpdate, nil)
	<-writerDone

	logger.Info("shutting down",
		zap.Int64("points_written", writer.Written()),
		zap.Int64("points_dropped", writer.Dropped()))
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
