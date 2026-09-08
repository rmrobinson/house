package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
	"go.uber.org/zap"
)

// discoveryTimeout bounds the startup mDNS browse. Cast devices answer
// within a few hundred ms on a healthy LAN; this leaves headroom for a
// slow/busy network without hanging startup indefinitely.
const discoveryTimeout = 3 * time.Second

// lookupFunc performs an mDNS query, matching mdns.QueryContext's signature.
// Overridable in tests so discoverDevices doesn't need a real mDNS responder
// on the network.
type lookupFunc func(ctx context.Context, params *mdns.QueryParam) error

// mdnsLogWriter adapts hashicorp/mdns's *log.Logger output into the
// bridge's structured zap logging. Every line goes through Warn — the
// library only writes when something's actually gone wrong internally (a
// bind failure, a malformed response), never for routine operation.
type mdnsLogWriter struct {
	logger *zap.Logger
}

func (w mdnsLogWriter) Write(p []byte) (int, error) {
	w.logger.Warn("mdns", zap.String("msg", strings.TrimRight(string(p), "\n")))
	return len(p), nil
}

// discoverDevices performs a single mDNS browse of _googlecast._tcp and
// returns every responding device as a deviceConfig, built from the same TXT
// record fields DeviceKind (devices.go) already parses for "ca" plus "id"
// for the device's UUID and "fn" for its mDNS friendly name. Entries with no
// "id" field are skipped — without a UUID there's nothing stable to key the
// device on across restarts/reconnects.
func discoverDevices(ctx context.Context, logger *zap.Logger, lookup lookupFunc) []deviceConfig {
	entries := make(chan *mdns.ServiceEntry, 16)
	done := make(chan struct{})
	var found []deviceConfig

	go func() {
		defer close(done)
		for e := range entries {
			cfg, ok := deviceConfigFromEntry(e)
			if !ok {
				logger.Debug("ignoring mDNS entry missing a device id or address", zap.String("name", e.Name))
				continue
			}
			logger.Info("discovered cast device",
				zap.String("uuid", cfg.UUID), zap.String("host", cfg.Host),
				zap.String("name", cfg.Name), zap.String("kind", cfg.Kind))
			found = append(found, cfg)
		}
	}()

	params := &mdns.QueryParam{
		Service: "_googlecast._tcp",
		Domain:  "local",
		Timeout: discoveryTimeout,
		Entries: entries,
		// hashicorp/mdns logs its own failures (e.g. "failed to bind to udp6
		// port") straight to a *log.Logger, bypassing zap entirely — observed
		// live on a network without IPv6 multicast support, printed directly
		// to stderr outside any of the bridge's own structured logging.
		// Route it through zap instead, and skip IPv6 multicast up front
		// (DisableIPv6 only affects how the query is sent/listened for, not
		// which discovered addresses are usable — see QueryParam's doc
		// comment) since the bind attempt is failing anyway on networks like
		// the one this was tested against, and Cast devices are IPv4 in
		// practice.
		Logger:      log.New(mdnsLogWriter{logger: logger}, "", 0),
		DisableIPv6: true,
	}
	if err := lookup(ctx, params); err != nil {
		logger.Warn("mDNS discovery failed", zap.Error(err))
	}
	close(entries)
	<-done

	return found
}

// deviceConfigFromEntry converts a raw mDNS service entry into a
// deviceConfig, or reports false if the entry can't be turned into one (no
// "id" TXT field, or no address to dial).
func deviceConfigFromEntry(e *mdns.ServiceEntry) (deviceConfig, bool) {
	txt := parseTXT(e.InfoFields)

	uuid := txt["id"]
	if uuid == "" {
		return deviceConfig{}, false
	}

	host := ""
	switch {
	case e.AddrV4 != nil:
		host = e.AddrV4.String()
	case e.Addr != nil: //nolint:staticcheck // Addr is deprecated upstream but still the only IPv6 fallback available
		host = e.Addr.String()
	}
	if host == "" {
		return deviceConfig{}, false
	}

	kind := "media_player"
	if DeviceKind(txt) == KindTelevision {
		kind = "television"
	}

	return deviceConfig{
		UUID: uuid,
		Host: host,
		Name: txt["fn"],
		Kind: kind,
	}, true
}

// parseTXT splits mDNS TXT record fields ("key=value" strings, per
// ServiceEntry.InfoFields) into a map, the same shape DeviceKind expects.
// Fields without an "=" are dropped rather than erroring — TXT records are
// best-effort metadata, not a contract worth failing discovery over.
func parseTXT(fields []string) map[string]string {
	txt := make(map[string]string, len(fields))
	for _, f := range fields {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		txt[k] = v
	}
	return txt
}

// mergeDiscovered folds discovered into configured per house's discovery
// contract: config-listed devices are authoritative, so a discovered device
// already present (matched by UUID) only gets its Host refreshed — Name and
// Kind stay whatever the operator configured, even if mDNS disagrees.
// Discovered devices with a UUID not already configured are appended.
// Nothing is ever removed: a device missing from this particular browse
// (mDNS is inherently best-effort/lossy) stays exactly as configured, and a
// previously-discovered device isn't dropped just because this run's browse
// didn't see it — discoverDevices only runs once, at startup, so there's no
// "later browse" to compare against anyway.
func mergeDiscovered(configured, discovered []deviceConfig) []deviceConfig {
	byUUID := make(map[string]int, len(configured))
	for i, c := range configured {
		byUUID[c.UUID] = i
	}

	// Copied rather than aliased: mutating configured's backing array in
	// place here would be a surprising side effect for any future caller
	// that keeps its own reference to the slice it passed in.
	merged := append([]deviceConfig(nil), configured...)
	for _, d := range discovered {
		if i, ok := byUUID[d.UUID]; ok {
			merged[i].Host = d.Host
			continue
		}
		merged = append(merged, d)
		byUUID[d.UUID] = len(merged) - 1
	}
	return merged
}
