package wol

import (
	"net"
	"syscall"
)

// sendUDP broadcasts/unicasts the payload to each dest:port. A single socket
// with SO_BROADCAST enabled handles both broadcast and unicast destinations.
func sendUDP(payload []byte, dests []string, ports []int) (int, error) {
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return 0, err
	}
	defer pc.Close()

	if sc, err := pc.SyscallConn(); err == nil {
		sc.Control(func(fd uintptr) {
			syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
		})
	}

	var sent int
	var firstErr error
	for _, d := range dests {
		ip := net.ParseIP(d)
		if ip == nil {
			continue
		}
		for _, p := range ports {
			if _, err := pc.WriteToUDP(payload, &net.UDPAddr{IP: ip, Port: p}); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			sent++
		}
	}
	return sent, firstErr
}
