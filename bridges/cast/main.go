package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/mdns"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigName("cast")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/etc/house")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	viper.SetDefault("bridge.listen_port", 17011)

	if err := viper.ReadInConfig(); err != nil {
		logger.Fatal("unable to read config", zap.Error(err))
	}

	if len(viper.GetString("bridge.id")) < 1 {
		bridgeID := uuid.New().String()

		logger.Info("config missing bridge id, saving new bridge id",
			zap.String("bridge_id", bridgeID))

		viper.Set("bridge.id", bridgeID)

		if err := viper.WriteConfig(); err != nil {
			logger.Fatal("unable to write new config", zap.Error(err))
		}
	}

	var deviceConfigs []deviceConfig
	if err := viper.UnmarshalKey("cast.devices", &deviceConfigs); err != nil {
		logger.Fatal("unable to parse cast.devices config", zap.Error(err))
	}

	// Opt-in (default false): a single mDNS browse for _googlecast._tcp at
	// startup. Config-listed devices stay authoritative — discovery only
	// appends UUIDs it doesn't already know about and refreshes the Host of
	// ones it does (DHCP leases change); see mergeDiscovered's doc comment.
	// This is what lets cast.devices be omitted entirely rather than
	// hand-maintained, per cast.discovery.mdns_enabled in cast.example.yaml.
	if viper.GetBool("cast.discovery.mdns_enabled") {
		discoverCtx, cancel := context.WithTimeout(context.Background(), discoveryTimeout+time.Second)
		discovered := discoverDevices(discoverCtx, logger, mdns.QueryContext)
		cancel()
		deviceConfigs = mergeDiscovered(deviceConfigs, discovered)
	}

	if len(deviceConfigs) == 0 {
		logger.Fatal("no cast devices configured or discovered; set cast.devices and/or enable cast.discovery.mdns_enabled")
	}

	svc := bridge.NewService(logger)

	cb := NewCastBridge(logger, svc, deviceConfigs)
	svc.RegisterHandler(cb, cb.b)

	// The art proxy is optional: it only starts if cast.art_base_url is set,
	// e.g. "http://192.168.1.50:17012" — the address this bridge is reachable
	// at from the dashboard's perspective. There's no reliable way to infer
	// that from inside the process, so it's config, not autodetected.
	//
	// This must happen before cb.Start below: Start launches every device's
	// connection goroutine, which can push a device update (with art URLs to
	// rewrite) as soon as the first RECEIVER_STATUS/MEDIA_STATUS arrives. If
	// the art proxy weren't set yet, that first update would go out with raw
	// upstream URLs instead of proxied ones.
	artBaseURL := viper.GetString("cast.art_base_url")
	if artBaseURL != "" {
		artListenPort := viper.GetInt("cast.art_listen_port")
		if artListenPort == 0 {
			artListenPort = 17012
		}

		artProxy := NewArtProxy(logger, artBaseURL)
		cb.SetArtProxy(artProxy)

		mux := http.NewServeMux()
		mux.Handle("/art/", artProxy)

		logger.Info("listening for art proxy requests", zap.Int("port", artListenPort))
		go http.ListenAndServe(fmt.Sprintf(":%d", artListenPort), mux) //nolint:errcheck
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	s := bridge.NewServer(logger, svc)
	s.ServeOnPort(viper.GetInt("bridge.listen_port"))
}
