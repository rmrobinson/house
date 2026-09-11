package homekitctrl

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

const (
	hapServiceName   = "_hap._tcp"
	hapServiceDomain = "local."
)

// DiscoveredAccessory is a HAP accessory found via mDNS.
type DiscoveredAccessory struct {
	Name      string
	PairingID string
	Model     string
	IPs       []net.IP
	Port      int
}

// Addr returns host:port suitable for Connect, preferring the first advertised IPv4 address
// (accessories often advertise several addresses at once - IPv4 plus multiple IPv6
// link-local/ULA - and IPv4 is the least likely to be affected by scope-id ambiguity).
func (d DiscoveredAccessory) Addr() (string, error) {
	for _, ip := range d.IPs {
		if ip.To4() != nil {
			return net.JoinHostPort(ip.String(), strconv.Itoa(d.Port)), nil
		}
	}
	if len(d.IPs) > 0 {
		return net.JoinHostPort(d.IPs[0].String(), strconv.Itoa(d.Port)), nil
	}
	return "", fmt.Errorf("accessory %s advertised no addresses", d.Name)
}

// Discover browses for HAP accessories for up to timeout, calling onFound for each one seen.
func Discover(ctx context.Context, timeout time.Duration, onFound func(DiscoveredAccessory)) error {
	resolver, err := zeroconf.NewResolver()
	if err != nil {
		return fmt.Errorf("init mDNS resolver: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	entries := make(chan *zeroconf.ServiceEntry)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Exits on either entries closing (zeroconf's own signal that browsing ended) or ctx
		// being cancelled directly - the latter matters because on a Browse error below we
		// cancel ctx ourselves rather than closing entries, since Browse's error contract
		// doesn't guarantee it hasn't retained the channel (closing/sending on it from both
		// sides risks a panic).
		for {
			select {
			case entry, ok := <-entries:
				if !ok {
					return
				}
				onFound(discoveredAccessoryFromEntry(entry))
			case <-ctx.Done():
				return
			}
		}
	}()

	if err := resolver.Browse(ctx, hapServiceName, hapServiceDomain, entries); err != nil {
		cancel()
		<-done
		return fmt.Errorf("browse for %s: %w", hapServiceName, err)
	}

	<-ctx.Done()
	<-done
	return nil
}

// DeviceByID browses until an accessory advertising the given HAP pairing ID responds, or
// timeout elapses. Always re-resolve via this rather than trusting a previously cached
// address - HAP accessories can move IPs (DHCP), and this is the one and only piece of the
// original design's cross-VLAN discovery concern (mDNS reflector) actually needed in-bridge:
// none, since the deployment already reflects mDNS at the network level.
func DeviceByID(ctx context.Context, pairingID string, timeout time.Duration) (*DiscoveredAccessory, error) {
	found := make(chan DiscoveredAccessory, 1)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	err := Discover(ctx, timeout, func(d DiscoveredAccessory) {
		if d.PairingID != pairingID {
			return
		}
		select {
		case found <- d:
			cancel()
		default:
		}
	})
	if err != nil {
		return nil, err
	}

	select {
	case d := <-found:
		return &d, nil
	default:
		return nil, fmt.Errorf("accessory %q not found within %s", pairingID, timeout)
	}
}

func discoveredAccessoryFromEntry(entry *zeroconf.ServiceEntry) DiscoveredAccessory {
	txt := make(map[string]string, len(entry.Text))
	for _, kv := range entry.Text {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			txt[strings.ToLower(parts[0])] = parts[1]
		}
	}

	return DiscoveredAccessory{
		Name:      entry.Instance,
		PairingID: txt["id"],
		Model:     txt["md"],
		IPs:       append(append([]net.IP{}, entry.AddrIPv4...), entry.AddrIPv6...),
		Port:      entry.Port,
	}
}
