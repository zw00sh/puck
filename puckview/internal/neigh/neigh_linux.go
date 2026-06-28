//go:build linux

package neigh

import (
	"encoding/binary"
	"log"
	"net"
	"sync"

	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// warnFallback logs (once) when the netlink neigh read fails and we drop to
// /proc/net/arp — a degraded mode with no NUD state granularity or last-seen.
// Read() runs every tick, so a one-time warning avoids journal spam while still
// surfacing a sandbox/permission regression (e.g. AF_NETLINK not allowed).
var warnFallback sync.Once

// NUD (neighbour unreachability detection) states.
const (
	nudIncomplete = 0x01
	nudReachable  = 0x02
	nudStale      = 0x04
	nudDelay      = 0x08
	nudProbe      = 0x10
	nudFailed     = 0x20
	nudNoarp      = 0x40
	nudPermanent  = 0x80
)

// NDA attribute types.
const (
	ndaDst       = 1
	ndaLLAddr    = 2
	ndaCacheInfo = 3
)

func read() ([]Entry, error) {
	if e, err := readNetlink(); err == nil {
		return e, nil
	} else {
		warnFallback.Do(func() {
			log.Printf("neigh: netlink read failed (%v); falling back to /proc/net/arp — no REACHABLE state or last-seen. Ensure the service may open AF_NETLINK sockets.", err)
		})
	}
	// Fallback: /proc/net/arp (no last-seen).
	return readProc()
}

func readNetlink() ([]Entry, error) {
	c, err := netlink.Dial(unix.NETLINK_ROUTE, nil)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	// struct ndmsg: family(1) pad1(1) pad2(2) ifindex(4) state(2) flags(1) type(1)
	req := make([]byte, 12)
	req[0] = unix.AF_INET

	msgs, err := c.Execute(netlink.Message{
		Header: netlink.Header{
			Type:  unix.RTM_GETNEIGH,
			Flags: netlink.Request | netlink.Dump,
		},
		Data: req,
	})
	if err != nil {
		return nil, err
	}

	var out []Entry
	for _, m := range msgs {
		if len(m.Data) < 12 {
			continue
		}
		state := binary.LittleEndian.Uint16(m.Data[8:10])
		ad, err := netlink.NewAttributeDecoder(m.Data[12:])
		if err != nil {
			continue
		}
		var e Entry
		e.SeenSec = -1
		e.State = nudToState(state)
		for ad.Next() {
			switch ad.Type() {
			case ndaDst:
				if b := ad.Bytes(); len(b) == 4 {
					e.IP = net.IP(b).String()
				}
			case ndaLLAddr:
				if b := ad.Bytes(); len(b) == 6 {
					e.MAC = net.HardwareAddr(b).String()
				}
			case ndaCacheInfo:
				// struct nda_cacheinfo: confirmed, used, updated, refcnt (u32 each),
				// time deltas in units of 1/USER_HZ (100) seconds.
				if b := ad.Bytes(); len(b) >= 4 {
					confirmed := binary.LittleEndian.Uint32(b[0:4])
					e.SeenSec = int64(confirmed) / 100
				}
			}
		}
		if e.IP == "" || e.MAC == "" {
			continue // incomplete entries (no LL addr yet) are not useful here
		}
		out = append(out, e)
	}
	return out, nil
}

func nudToState(state uint16) string {
	switch {
	case state&(nudReachable|nudPermanent|nudNoarp) != 0:
		return "up"
	case state&(nudStale|nudDelay|nudProbe) != 0:
		return "stale"
	default: // FAILED, INCOMPLETE, NONE
		return "down"
	}
}
