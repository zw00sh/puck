// Package wol sends Wake-on-LAN magic packets in every form we learned a target
// might need: an L2 frame (ethertype 0x0842), a subnet-directed broadcast, the
// limited broadcast 255.255.255.255, and a unicast — each on UDP ports 9 and 7.
// Broadcast addressing is derived from the interface, never a user-entered
// netmask (the hand-entered netmask was a silent WoL failure).
package wol

import (
	"fmt"
	"net"
)

// wakePorts are the conventional discard/echo ports WoL listens on.
var wakePorts = []int{9, 7}

// Waker holds the interface-derived addressing used to build the barrage.
type Waker struct {
	Iface     string // LAN interface name (for the L2 send)
	Broadcast string // subnet-directed broadcast, e.g. 192.168.50.255
}

// MagicPayload builds the WoL payload: 6×0xFF followed by the target MAC ×16.
func MagicPayload(mac net.HardwareAddr) []byte {
	buf := make([]byte, 0, 6+16*6)
	for i := 0; i < 6; i++ {
		buf = append(buf, 0xFF)
	}
	for i := 0; i < 16; i++ {
		buf = append(buf, mac...)
	}
	return buf
}

// Send fires one full barrage at the target. The return count is the number of
// packets emitted (used for the UI's live packet counter). Errors from
// individual sends are non-fatal — partial barrages still routinely wake hosts —
// so they are aggregated and returned for diagnostics without aborting.
func (wk Waker) Send(mac net.HardwareAddr, unicastIP string) (int, error) {
	payload := MagicPayload(mac)
	var sent int
	var errs []error

	// L2 frame (ethertype 0x0842) — platform-specific (Linux only).
	if n, err := sendL2(wk.Iface, mac, payload); err != nil {
		errs = append(errs, fmt.Errorf("l2: %w", err))
	} else {
		sent += n
	}

	// UDP forms on each wake port.
	dests := []string{}
	if wk.Broadcast != "" {
		dests = append(dests, wk.Broadcast)
	}
	dests = append(dests, "255.255.255.255")
	if unicastIP != "" {
		dests = append(dests, unicastIP)
	}
	n, err := sendUDP(payload, dests, wakePorts)
	sent += n
	if err != nil {
		errs = append(errs, fmt.Errorf("udp: %w", err))
	}

	if len(errs) > 0 {
		return sent, errs[0]
	}
	return sent, nil
}
