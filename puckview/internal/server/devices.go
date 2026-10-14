package server

import (
	"net"

	"github.com/zw00sh/puck/puckview/internal/neigh"
	"github.com/zw00sh/puck/puckview/internal/oui"
)

// View payloads sent to the browser. JSON shapes match what app.js renders.

type icmpResult struct {
	St  string `json:"st"`  // "up" | "down"
	RTT int    `json:"rtt"` // ms; always emitted (a sub-ms reply is 0, not absent)
}

type probeView struct {
	Port int    `json:"port"`
	St   string `json:"st"`  // "up" | "down"
	RTT  int    `json:"rtt"` // ms; always emitted so an open-in-0ms probe isn't "undefined"
	Note string `json:"note,omitempty"`
}

type deviceView struct {
	MAC    string      `json:"mac"`
	IP     string      `json:"ip"`
	Name   string      `json:"name"`
	Vendor string      `json:"vendor"`
	Seen   string      `json:"seen"`
	Arp    string      `json:"arp"` // up | stale | down
	Icmp   *icmpResult `json:"icmp"`
	Probes []probeView `json:"probes"`
	// Seen is time since the host was last confirmed UP (ICMP/TCP). SeenL2 is set
	// when Seen is an ARP-presence fallback (host never verified up) — the UI
	// marks it so an L2-only sighting isn't mistaken for a real response.
	SeenL2 bool `json:"seenL2"`
	// UnseenSec is seconds since the host was last confirmed up (0 if up now; for
	// a never-up device, seconds since it was tracked). Drives the DOWN banner's
	// "unseen for ≥60s" debounce.
	UnseenSec int `json:"unseenSec"`
	// IcmpSeen: this device has answered ICMP at least once. With watchdogs, it
	// marks a device we'd expect to be reachable beyond L2 — so an ARP-only
	// presence reads as suspect (likely asleep) rather than merely present.
	IcmpSeen bool `json:"icmpSeen"`
}

type cacheView struct {
	IP     string `json:"ip"`
	MAC    string `json:"mac"`
	Name   string `json:"name"`
	Vendor string `json:"vendor"`
	State  string `json:"state"`
	Seen   string `json:"seen"`
}

// neighSnapshot reads the kernel neighbour cache and indexes it by MAC. Passive
// (no packets), so it is safe regardless of the activity gate.
func neighSnapshot() (map[string]neigh.Entry, []neigh.Entry) {
	entries, err := neigh.Read()
	if err != nil {
		return map[string]neigh.Entry{}, nil
	}
	byMAC := make(map[string]neigh.Entry, len(entries))
	for _, e := range entries {
		// Prefer the freshest record per MAC.
		if prev, ok := byMAC[e.MAC]; !ok || better(e, prev) {
			byMAC[e.MAC] = e
		}
	}
	return byMAC, entries
}

var stateRank = map[string]int{"up": 3, "stale": 2, "down": 1}

func better(a, b neigh.Entry) bool {
	return stateRank[a.State] > stateRank[b.State]
}

// buildDevices assembles the tracked-device view from the in-memory model. Pure
// CPU over a snapshot — no database access, which is what lets it run every
// second (and on every connect) without the box noticing.
func (s *Server) buildDevices() []deviceView {
	now := nowUnix()
	snap := s.state.snapshot()
	out := make([]deviceView, 0, len(snap))
	for _, ds := range snap {
		v := deviceView{
			MAC: ds.dev.MAC, IP: ds.dev.IP, Name: ds.dev.Name,
			Vendor: ds.dev.Vendor, Arp: ds.arp, IcmpSeen: ds.icmpSeen,
			Probes: []probeView{},
		}
		if v.Vendor == "" {
			v.Vendor = s.vendorOf(ds.dev.MAC)
		}
		v.Icmp = ds.live.icmp
		if ds.live.probes != nil {
			v.Probes = ds.live.probes
		}
		if len(v.Probes) == 0 {
			for _, p := range ds.dev.Probes {
				v.Probes = append(v.Probes, probeView{Port: p, St: "down"})
			}
		}
		v.Seen, v.SeenL2, v.UnseenSec = seenState(ds, v, now)
		out = append(out, v)
	}
	return out
}

// deviceUp reports whether a device has a live "up" signal.
func deviceUp(v deviceView) bool { return liveUp(v.Icmp, v.Probes) }

// liveUp reports whether an active probe set shows the host awake: an ICMP reply
// or any open TCP watchdog. ARP is presence, not liveness, so it doesn't count.
func liveUp(icmp *icmpResult, probes []probeView) bool {
	if icmp != nil && icmp.St == "up" {
		return true
	}
	for _, p := range probes {
		if p.St == "up" {
			return true
		}
	}
	return false
}

// seenState computes the SEEN value, its L2-fallback flag, and UnseenSec for a
// tracked device. SEEN is time since the host was last confirmed UP (ICMP/TCP).
// If it has never verified up, only a pure-presence device (no watchdogs, never
// answered ICMP) falls back to its last L2 (ARP) presence, marked; a device we
// expect liveness from shows "—" until actually verified up (ARP is presence,
// not liveness). UnseenSec is seconds since last confirmed up (since-tracked for
// never-up devices), for the DOWN banner's debounce.
func seenState(ds devState, v deviceView, now int64) (seen string, l2 bool, unseenSec int) {
	if deviceUp(v) {
		return "now", false, 0
	}
	if ds.lastSeenUp > 0 {
		age := now - ds.lastSeenUp
		return neigh.FmtSeen(age), false, int(age)
	}
	// Never verified up. UnseenSec is measured from when the device was tracked so
	// a fresh add gets the same grace.
	unseenSec = int(now - ds.dev.CreatedAt)
	// A device we expect real liveness from — one with watchdogs, or that has
	// answered ICMP before — must not show ARP presence as a sighting: ARP is
	// presence, not liveness. Leave SEEN empty until it's actually verified up.
	// Only a pure-presence device (nothing expected of it) falls back to L2 (ARP).
	if len(ds.dev.Probes) == 0 && !ds.icmpSeen {
		if ds.arpSeen >= 0 {
			return neigh.FmtSeen(ds.arpSeen), true, unseenSec
		}
		if ds.lastSeen > 0 {
			return neigh.FmtSeen(now - ds.lastSeen), true, unseenSec
		}
	}
	return "—", false, unseenSec
}

// observedTTL is how long an untracked host lingers in the ARP cache view after
// it drops out of the kernel neigh cache.
const observedTTL = 7 * 24 * 60 * 60 // 1 week, seconds

// buildCache assembles the ARP-cache view: currently-present untracked neighbour
// entries, plus untracked hosts seen within observedTTL that have since dropped
// out of the kernel cache (shown as down with their last-seen).
func (s *Server) buildCache(entries []neigh.Entry) []cacheView {
	now := nowUnix()
	tracked := s.state.tracked()
	present := map[string]bool{}
	out := []cacheView{}

	// 1) Present now (live kernel neigh cache).
	for _, e := range entries {
		if tracked[e.MAC] || present[e.MAC] || !s.onLAN(e.IP) {
			continue
		}
		present[e.MAC] = true
		name := s.cachedName(e.IP)
		out = append(out, cacheView{
			IP: e.IP, MAC: e.MAC, Name: name,
			Vendor: s.vendorOf(e.MAC), State: e.State, Seen: neigh.FmtSeen(e.SeenSec),
		})
		s.state.touchObserved(e.MAC, e.IP, name, now) // remembered for the view below
	}

	// 2) Recently seen but gone now (within the retention window).
	for _, o := range s.state.observedSince(now - observedTTL) {
		if tracked[o.MAC] || present[o.MAC] {
			continue
		}
		name := o.Name
		if name == "" {
			name = s.cachedName(o.IP)
		}
		out = append(out, cacheView{
			IP: o.IP, MAC: o.MAC, Name: name,
			Vendor: s.vendorOf(o.MAC), State: "down", Seen: neigh.FmtSeen(now - o.LastSeen),
		})
	}
	return out
}

// onLAN reports whether ip is a real unicast host on the box's LAN subnet. It
// filters out multicast/broadcast and entries belonging to other interfaces
// (VM/tunnel adapters) that also sit in the kernel neigh table. If the LAN
// subnet is unknown (interface detection failed), it only drops multicast.
func (s *Server) onLAN(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil || ip.IsMulticast() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
		return false
	}
	if n := s.ni.IPNet(); n != nil {
		return n.Contains(ip)
	}
	return true
}

// vendorOf resolves a MAC's vendor from the embedded OUI table (passive, local).
func (s *Server) vendorOf(mac string) string { return oui.Lookup(mac) }
