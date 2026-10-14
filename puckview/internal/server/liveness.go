package server

import (
	"sync"
	"time"

	"github.com/zw00sh/puck/puckview/internal/probe"
)

const probeTimeout = 700 * time.Millisecond

type livePart struct {
	icmp   *icmpResult
	probes []probeView
}

// probeICMP runs one ICMP liveness probe and maps it to the view shape.
func probeICMP(ip string) *icmpResult {
	if up, rtt := probe.ICMP(ip, probeTimeout); up {
		return &icmpResult{St: "up", RTT: rtt}
	}
	return &icmpResult{St: "down"}
}

// probeOne runs one TCP watchdog probe and maps it to the view shape.
func probeOne(ip string, port int) probeView {
	if open, rtt := probe.TCP(ip, port, probeTimeout); open {
		return probeView{Port: port, St: "up", RTT: rtt}
	}
	return probeView{Port: port, St: "down", Note: "closed"}
}

// probeTracked actively probes every tracked device (ICMP + each TCP watchdog)
// and folds the results into the in-memory model. This is gated network traffic:
// the probe loop calls it only while the activity gate is open.
//
// Results used to be written to the database here — a row per up device per
// cycle, which is what grew the old seen_history log to 1.6M rows. Presence now
// lives in memory and is flushed on a slow cadence; see state.applyProbes.
func (s *Server) probeTracked() {
	snap := s.state.snapshot()
	results := make(map[string]livePart, len(snap))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, ds := range snap {
		if ds.dev.IP == "" {
			continue
		}
		wg.Add(1)
		go func(mac, ip string, ports []int) {
			defer wg.Done()
			var part livePart
			part.icmp = probeICMP(ip)
			for _, p := range ports {
				part.probes = append(part.probes, probeOne(ip, p))
			}
			mu.Lock()
			results[mac] = part
			mu.Unlock()
		}(ds.dev.MAC, ds.dev.IP, ds.dev.Probes)
	}
	wg.Wait()
	s.state.applyProbes(results, nowUnix())
}
