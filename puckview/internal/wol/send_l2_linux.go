//go:build linux

package wol

import (
	"net"

	"github.com/mdlayher/packet"
)

const etherTypeWOL = 0x0842

// sendL2 emits the magic packet as a raw Ethernet frame with the WOL ethertype,
// broadcast at L2. This is the firewall-proof form and needs CAP_NET_RAW.
func sendL2(iface string, mac net.HardwareAddr, payload []byte) (int, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return 0, err
	}
	conn, err := packet.Listen(ifi, packet.Raw, etherTypeWOL, nil)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	bcast := net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	frame := make([]byte, 0, 14+len(payload))
	frame = append(frame, bcast...)            // dst
	frame = append(frame, ifi.HardwareAddr...) // src
	frame = append(frame, 0x08, 0x42)          // ethertype
	frame = append(frame, payload...)

	if _, err := conn.WriteTo(frame, &packet.Addr{HardwareAddr: bcast}); err != nil {
		return 0, err
	}
	return 1, nil
}
