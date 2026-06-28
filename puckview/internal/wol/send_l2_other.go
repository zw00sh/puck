//go:build !linux

package wol

import "net"

// sendL2 is a no-op off Linux (the dev box). The UDP forms still go out, which
// is enough to wake a host on the same segment during local testing.
func sendL2(iface string, mac net.HardwareAddr, payload []byte) (int, error) {
	return 0, nil
}
