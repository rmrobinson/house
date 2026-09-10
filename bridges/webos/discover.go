package main

import (
	"net"
	"net/url"
	"strings"

	"github.com/koron/go-ssdp"
	"go.uber.org/zap"
)

// searchTarget is the SSDP ST used to find webOS TVs' second-screen service.
const searchTarget = "urn:lge-com:service:webos-second-screen:1"

// discoveryWaitSec bounds how long a single M-SEARCH waits for responses.
const discoveryWaitSec = 3

// discovered is one device found by an SSDP scan.
type discovered struct {
	// UUID is the device's stable identity, extracted from the USN header
	// ("uuid:<UUID>::urn:lge-com:service:webos-second-screen:1"). Per the
	// handoff doc, this UUID *is* the device id - there's no separate
	// house-side id mapping layer.
	UUID string
	Host string
}

// searchFunc performs an SSDP M-SEARCH, matching ssdp.Search's signature
// (minus its variadic opts, which nothing here needs to set). Overridable in
// tests so discoverDevices doesn't need a real network responder.
type searchFunc func(searchType string, waitSec int, localAddr string) ([]ssdp.Service, error)

// ssdpSearch adapts ssdp.Search to searchFunc.
func ssdpSearch(searchType string, waitSec int, localAddr string) ([]ssdp.Service, error) {
	return ssdp.Search(searchType, waitSec, localAddr)
}

// discoverDevices performs a single SSDP M-SEARCH for webOS TVs and returns
// every device that answered with a well-formed USN and a resolvable host.
func discoverDevices(logger *zap.Logger, search searchFunc) []discovered {
	services, err := search(searchTarget, discoveryWaitSec, "")
	if err != nil {
		logger.Warn("SSDP discovery failed", zap.Error(err))
		return nil
	}

	var found []discovered
	for _, svc := range services {
		uuid, ok := uuidFromUSN(svc.USN)
		if !ok {
			logger.Debug("ignoring SSDP response missing a usable uuid", zap.String("usn", svc.USN))
			continue
		}
		host, ok := hostFromLocation(svc.Location)
		if !ok {
			logger.Debug("ignoring SSDP response missing a usable location", zap.String("location", svc.Location))
			continue
		}
		logger.Info("discovered webos device", zap.String("uuid", uuid), zap.String("host", host))
		found = append(found, discovered{UUID: uuid, Host: host})
	}
	return found
}

// uuidFromUSN extracts the UUID from a USN of the form
// "uuid:<UUID>::urn:lge-com:service:webos-second-screen:1".
func uuidFromUSN(usn string) (string, bool) {
	if !strings.HasPrefix(usn, "uuid:") {
		return "", false
	}
	rest := strings.TrimPrefix(usn, "uuid:")
	uuid, _, ok := strings.Cut(rest, ":")
	if !ok || uuid == "" {
		return "", false
	}
	return uuid, true
}

// hostFromLocation extracts the bare host (no port) from an SSDP LOCATION
// URL.
func hostFromLocation(location string) (string, bool) {
	u, err := url.Parse(location)
	if err != nil {
		return "", false
	}
	host := u.Hostname()
	if host == "" || net.ParseIP(host) == nil {
		return "", false
	}
	return host, true
}
