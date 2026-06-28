// Package probe performs the active liveness checks: a TCP connect (the
// watchdog) and an ICMP echo. These emit packets and so run only while a
// dashboard client is connected. ICMP is treated as best-effort — many hosts
// (notably Windows) drop echoes, which is exactly why liveness is multi-signal.
package probe

import (
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// TCP attempts a connect to ip:port. Returns whether it opened and the RTT (ms).
func TCP(ip string, port int, timeout time.Duration) (bool, int) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, fmt.Sprint(port)), timeout)
	if err != nil {
		return false, 0
	}
	conn.Close()
	return true, int(time.Since(start).Milliseconds())
}

// ICMP sends a single echo request and waits for the reply. It first tries an
// unprivileged datagram socket (works on macOS and on Linux when the ping group
// range permits) and falls back to a raw socket (needs CAP_NET_RAW). Returns
// whether a reply came back and the RTT (ms).
func ICMP(ip string, timeout time.Duration) (bool, int) {
	dst := net.ParseIP(ip)
	if dst == nil || dst.To4() == nil {
		return false, 0
	}

	network, listen := "udp4", "0.0.0.0"
	conn, err := icmp.ListenPacket(network, listen)
	if err != nil {
		network = "ip4:icmp"
		conn, err = icmp.ListenPacket(network, listen)
		if err != nil {
			return false, 0
		}
	}
	defer conn.Close()

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{ID: os.Getpid() & 0xffff, Seq: 1, Data: []byte("puckview")},
	}
	b, err := msg.Marshal(nil)
	if err != nil {
		return false, 0
	}

	var dest net.Addr = &net.IPAddr{IP: dst}
	if network == "udp4" {
		dest = &net.UDPAddr{IP: dst}
	}

	start := time.Now()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.WriteTo(b, dest); err != nil {
		return false, 0
	}

	reply := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(reply)
		if err != nil {
			return false, 0
		}
		if !sameHost(peer, dst) {
			continue // not our reply; keep reading until deadline
		}
		rm, err := icmp.ParseMessage(1, reply[:n]) // 1 = IPv4 ICMP proto
		if err != nil {
			return false, 0
		}
		if rm.Type == ipv4.ICMPTypeEchoReply {
			return true, int(time.Since(start).Milliseconds())
		}
	}
}

func sameHost(peer net.Addr, ip net.IP) bool {
	switch a := peer.(type) {
	case *net.UDPAddr:
		return a.IP.Equal(ip)
	case *net.IPAddr:
		return a.IP.Equal(ip)
	}
	return false
}
