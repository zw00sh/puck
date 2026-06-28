package server

import (
	"sync"
	"time"

	"github.com/zw00sh/puck/puckview/internal/probe"
	"github.com/zw00sh/puck/puckview/internal/store"
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

// getLive returns the most recent active-probe results for a device (empty until
// the first probe of a session).
func (s *Server) getLive(mac, ip string, ports []int) (*icmpResult, []probeView) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	p, ok := s.liveResults[mac]
	if !ok {
		return nil, nil
	}
	return p.icmp, p.probes
}

// probeTracked actively probes every tracked device (ICMP + each TCP watchdog)
// and caches the results. This is gated network traffic: callers invoke it only
// while the activity gate is open.
func (s *Server) probeTracked() {
	devs, err := s.store.List()
	if err != nil {
		return
	}
	results := make(map[string]livePart, len(devs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, d := range devs {
		if d.IP == "" {
			continue
		}
		wg.Add(1)
		go func(d store.Device) {
			defer wg.Done()
			var part livePart
			part.icmp = probeICMP(d.IP)
			for _, p := range d.Probes {
				part.probes = append(part.probes, probeOne(d.IP, p))
			}
			mu.Lock()
			results[d.MAC] = part
			mu.Unlock()
			// Record a presence snapshot when any active signal answered, to
			// extend last-seen history beyond the kernel's neigh retention.
			if part.icmp != nil && part.icmp.St == "up" {
				s.store.RecordSeen(d.MAC, "up", "icmp")
			} else {
				for _, pv := range part.probes {
					if pv.St == "up" {
						s.store.RecordSeen(d.MAC, "up", "tcp")
						break
					}
				}
			}
		}(d)
	}
	wg.Wait()
	s.liveMu.Lock()
	s.liveResults = results
	s.liveMu.Unlock()
}
