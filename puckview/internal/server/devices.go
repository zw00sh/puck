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

// buildDevices assembles the tracked-device view from the store, the neighbour
// snapshot, and (optionally) active liveness results.
func (s *Server) buildDevices(byMAC map[string]neigh.Entry) []deviceView {
	devs, err := s.store.List()
	if err != nil {
		return nil
	}
	icmpSeen, _ := s.store.ICMPSeenSet()
	out := make([]deviceView, 0, len(devs))
	for _, d := range devs {
		v := deviceView{MAC: d.MAC, IP: d.IP, Name: d.Name, Vendor: d.Vendor, Probes: []probeView{}}
		v.IcmpSeen = icmpSeen[d.MAC]
		if v.Vendor == "" {
			v.Vendor = s.vendorOf(d.MAC)
		}

		arp := "down"
		arpSeenSec := int64(-1)
		if e, ok := byMAC[d.MAC]; ok {
			arp = e.State
			arpSeenSec = e.SeenSec
			// Persist the sighting (throttled) so the device still shows a useful
			// last-seen after it drops out of the kernel neigh cache.
			s.touchSeen(d.MAC, e.State)
			if e.IP != "" && e.IP != d.IP {
				// ARP reconciliation: the device moved IPs.
				s.store.ReconcileIP(d.MAC, e.IP)
				v.IP = e.IP
			}
		}
		v.Arp = arp

		icmp, probes := s.getLive(d.MAC, v.IP, d.Probes)
		v.Icmp = icmp
		if probes != nil {
			v.Probes = probes
		}
		if len(v.Probes) == 0 {
			for _, p := range d.Probes {
				v.Probes = append(v.Probes, probeView{Port: p, St: "down"})
			}
		}

		v.Seen, v.SeenL2, v.UnseenSec = s.seenState(d.MAC, v, arpSeenSec, d.CreatedAt)
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
// tracked device. SEEN is time since the host was last confirmed UP (ICMP/TCP);
// if it has never verified up, fall back to its last L2 (ARP) presence, marked.
// UnseenSec is seconds since last confirmed up (since-tracked for never-up
// devices), for the DOWN banner's debounce.
func (s *Server) seenState(mac string, v deviceView, arpSeenSec, createdAt int64) (seen string, l2 bool, unseenSec int) {
	if deviceUp(v) {
		return "now", false, 0
	}
	if ts := s.store.LastSeenUp(mac); ts > 0 {
		age := nowUnix() - ts
		return neigh.FmtSeen(age), false, int(age)
	}
	// Never verified up — fall back to L2 (ARP) presence, marked. UnseenSec is
	// measured from when the device was tracked so a fresh add gets the same grace.
	unseenSec = int(nowUnix() - createdAt)
	if arpSeenSec >= 0 {
		return neigh.FmtSeen(arpSeenSec), true, unseenSec
	}
	if ts := s.store.LastSeen(mac); ts > 0 {
		return neigh.FmtSeen(nowUnix() - ts), true, unseenSec
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
	tracked := map[string]bool{}
	if devs, err := s.store.List(); err == nil {
		for _, d := range devs {
			tracked[d.MAC] = true
		}
	}
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
		s.touchObserved(e.MAC, e.IP, name) // persist sighting for the recently-seen view
	}

	// 2) Recently seen but gone now (within the retention window).
	if obs, err := s.store.ListObserved(nowUnix() - observedTTL); err == nil {
		for _, o := range obs {
			if tracked[o.MAC] || present[o.MAC] {
				continue
			}
			name := o.Name
			if name == "" {
				name = s.cachedName(o.IP)
			}
			out = append(out, cacheView{
				IP: o.IP, MAC: o.MAC, Name: name,
				Vendor: s.vendorOf(o.MAC), State: "down", Seen: neigh.FmtSeen(nowUnix() - o.LastSeen),
			})
		}
	}
	return out
}

// touchObserved persists an untracked sighting (throttled to once/min per MAC).
func (s *Server) touchObserved(mac, ip, name string) {
	now := nowUnix()
	s.obsMu.Lock()
	if now-s.obsWrite[mac] < 60 {
		s.obsMu.Unlock()
		return
	}
	s.obsWrite[mac] = now
	s.obsMu.Unlock()
	s.store.UpsertObserved(mac, ip, name, now)
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

// touchSeen records a passive sighting of a tracked device, throttled to at most
// once per minute per device so seen_history doesn't bloat.
func (s *Server) touchSeen(mac, state string) {
	now := nowUnix()
	s.seenMu.Lock()
	last := s.seenWrite[mac]
	if now-last < 60 {
		s.seenMu.Unlock()
		return
	}
	s.seenWrite[mac] = now
	s.seenMu.Unlock()
	s.store.RecordSeen(mac, state, "arp")
}

// vendorOf resolves a MAC's vendor from the embedded OUI table (passive, local).
func (s *Server) vendorOf(mac string) string { return oui.Lookup(mac) }
