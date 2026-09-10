package webosctrl

import (
	"fmt"
	"net"
	"strings"
)

// SendMagicPacket broadcasts a Wake-on-LAN magic packet targeting mac (any of
// the usual "AA:BB:CC:DD:EE:FF" / "AA-BB-CC-DD-EE-FF" forms). Used to wake a
// TV that's in full standby (socket closed) rather than active-standby -
// which of the two a given set uses is a recon item, so the bridge always
// has this path available regardless of what one test TV exhibits.
func SendMagicPacket(mac string) error {
	hw, err := parseMAC(mac)
	if err != nil {
		return err
	}

	packet := make([]byte, 0, 102)
	for range 6 {
		packet = append(packet, 0xFF)
	}
	for range 16 {
		packet = append(packet, hw...)
	}

	conn, err := net.Dial("udp", "255.255.255.255:9")
	if err != nil {
		return fmt.Errorf("dialing broadcast address: %w", err)
	}
	defer conn.Close()

	_, err = conn.Write(packet)
	return err
}

func parseMAC(mac string) ([]byte, error) {
	mac = strings.ReplaceAll(mac, "-", ":")
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("parsing mac %q: %w", mac, err)
	}
	if len(hw) != 6 {
		return nil, fmt.Errorf("mac %q: expected 6 bytes, got %d", mac, len(hw))
	}
	return hw, nil
}
