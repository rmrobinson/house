package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/mdns"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/lib/configutil"
	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("bridge.listen_port", 17021)
	viper.SetDefault("cast.art_listen_port", 17023)
	viper.SetDefault("cast.media_listen_port", 17022)

	configPath, err := configutil.FindConfigFile("cast", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
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

	if len(viper.GetString("bridge.id")) < 1 {
		bridgeID := uuid.New().String()

		logger.Info("config missing bridge id, saving new bridge id",
			zap.String("bridge_id", bridgeID))

		viper.Set("bridge.id", bridgeID)

		if err := configutil.PersistValue(configPath, "bridge.id", bridgeID); err != nil {
			logger.Fatal("unable to write new config", zap.Error(err))
		}
	}

	// Every port this process listens on is resolved and checked here, so a
	// conflict is caught in one place at startup. The art proxy and media
	// server are optional and only claim their port when enabled.
	bridgePort := viper.GetInt("bridge.listen_port")
	artBaseURL := viper.GetString("cast.art_base_url")
	artPort := viper.GetInt("cast.art_listen_port")
	mediaDir := viper.GetString("cast.media_dir")
	mediaPort := viper.GetInt("cast.media_listen_port")

	claimed := map[int]string{bridgePort: "bridge.listen_port"}
	for _, l := range []struct {
		key     string
		port    int
		enabled bool
	}{
		{"cast.art_listen_port", artPort, artBaseURL != ""},
		{"cast.media_listen_port", mediaPort, mediaDir != ""},
	} {
		if !l.enabled {
			continue
		}
		if other, ok := claimed[l.port]; ok {
			logger.Fatal("listen ports must be distinct", zap.Int("port", l.port), zap.Strings("keys", []string{other, l.key}))
		}
		claimed[l.port] = l.key
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
	// e.g. "http://192.168.1.50:17023" — the address this bridge is reachable
	// at from the dashboard's perspective. There's no reliable way to infer
	// that from inside the process, so it's config, not autodetected.
	//
	// This must happen before cb.Start below: Start launches every device's
	// connection goroutine, which can push a device update (with art URLs to
	// rewrite) as soon as the first RECEIVER_STATUS/MEDIA_STATUS arrives. If
	// the art proxy weren't set yet, that first update would go out with raw
	// upstream URLs instead of proxied ones.
	if artBaseURL != "" {
		artProxy := NewArtProxy(logger, artBaseURL)
		cb.SetArtProxy(artProxy)

		mux := http.NewServeMux()
		mux.Handle("/art/", artProxy)

		// Bound here, not inside the goroutine, so a port already in use stops
		// the bridge now instead of silently leaving art URLs pointing nowhere.
		lis, err := net.Listen("tcp", fmt.Sprintf(":%d", artPort))
		if err != nil {
			logger.Fatal("unable to listen for art proxy requests", zap.Int("port", artPort), zap.Error(err))
		}

		logger.Info("listening for art proxy requests", zap.Int("port", artPort))
		go http.Serve(lis, mux) //nolint:errcheck
	}

	// The media server is optional too: cast.media_dir is a directory of files
	// to serve at http://<this host>:<cast.media_listen_port>/<file>, so a
	// PlayMedia content_url can point at a LAN copy instead of the internet.
	// It has its own port rather than sharing the art proxy's, which is
	// off by default and whose default port other bridges can hold.
	if mediaDir != "" {
		// Bind here rather than inside the goroutine: a port already in use
		// must stop the bridge now, not surface at 10pm as a speaker that can't
		// fetch its file.
		lis, err := net.Listen("tcp", fmt.Sprintf(":%d", mediaPort))
		if err != nil {
			logger.Fatal("unable to listen for media requests", zap.Int("port", mediaPort), zap.Error(err))
		}

		logger.Info("serving media files", zap.String("dir", mediaDir), zap.Int("port", mediaPort))
		go newMediaServer(mediaDir).Serve(lis) //nolint:errcheck
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	tlsCfg, err := bridge.TLSConfigFromViper()
	if err != nil {
		logger.Fatal("invalid bridge.tls config", zap.Error(err))
	}
	s, err := bridge.NewServer(logger, svc, tlsCfg)
	if err != nil {
		logger.Fatal("unable to create bridge server", zap.Error(err))
	}
	s.ServeOnPort(bridgePort)
}
