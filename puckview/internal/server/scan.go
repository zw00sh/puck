package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/zw00sh/puck/puckview/internal/naming"
	"github.com/zw00sh/puck/puckview/internal/scan"
	"github.com/zw00sh/puck/puckview/internal/store"
)

const resolveTimeout = 1200 * time.Millisecond

// lanResolver returns the DNS server to use for rDNS — explicit config, else the
// default gateway. Never the box's own (MagicDNS) resolver.
func (s *Server) lanResolver() string {
	if s.cfg.LANResolver != "" {
		return s.cfg.LANResolver
	}
	return s.ni.Gateway
}

// scanEvent is the SSE `scan` payload.
type scanEvent struct {
	State   string      `json:"state"` // progress | done | error
	Pct     int         `json:"pct"`
	Msg     string      `json:"msg"`
	Results []cacheView `json:"results,omitempty"`
}

// POST /api/scan {cidr} → 202; the sweep streams progress on SSE.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CIDR string `json:"cidr"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := scan.Validate(in.CIDR, s.ni.IPNet()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.scanMu.Lock()
	if s.scanning {
		s.scanMu.Unlock()
		writeErr(w, http.StatusConflict, "a scan is already running")
		return
	}
	s.scanning = true
	s.scanMu.Unlock()

	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"status":"started"}`))
	go s.runScan(in.CIDR)
}

func (s *Server) runScan(cidr string) {
	defer func() {
		s.scanMu.Lock()
		s.scanning = false
		s.scanMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	last := time.Now()
	entries, err := scan.Sweep(ctx, cidr, func(done, total int, cur string) {
		// Throttle progress frames to ~5/s to avoid flooding the SSE stream.
		if time.Since(last) < 200*time.Millisecond && done != total {
			return
		}
		last = time.Now()
		pct := 0
		if total > 0 {
			pct = done * 100 / total
		}
		s.hub.Broadcast("scan", scanEvent{State: "progress", Pct: pct, Msg: fmt.Sprintf("arp probe %s  (%d/%d)", cur, done, total)})
	})
	if err != nil {
		s.hub.Broadcast("scan", scanEvent{State: "error", Pct: 100, Msg: err.Error()})
		return
	}

	// Build results: in-subnet neighbours that aren't already tracked.
	tracked := map[string]bool{}
	if devs, e := s.store.List(); e == nil {
		for _, d := range devs {
			tracked[d.MAC] = true
		}
	}
	var ips []string
	for _, e := range entries {
		if !tracked[e.MAC] {
			ips = append(ips, e.IP)
		}
	}
	// Discovery is an explicit, user-initiated action — resolve names now.
	s.resolveNames(ips)

	results := []cacheView{}
	for _, e := range entries {
		if tracked[e.MAC] {
			continue
		}
		results = append(results, cacheView{
			IP: e.IP, MAC: e.MAC, Name: s.cachedName(e.IP),
			Vendor: s.vendorOf(e.MAC), State: e.State, Seen: "new",
		})
	}
	s.hub.Broadcast("scan", scanEvent{
		State: "done", Pct: 100,
		Msg:     fmt.Sprintf("done · %d new host(s)", len(results)),
		Results: results,
	})
	// Refresh the passive views now that the cache is warm.
	s.pushPresence()
}

// POST /api/devices/{mac}/resolve → re-run name resolution (active, gated by the
// user click). Returns the resolved result.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	mac := store.NormMAC(r.PathValue("mac"))
	dev, ok, err := s.store.Get(mac)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown device")
		return
	}
	res := naming.Resolve(dev.IP, dev.MAC, s.lanResolver(), resolveTimeout)
	s.applyResolve(mac, res)
	s.pushPresence()
	writeJSON(w, res)
}

// applyResolve persists resolved name/vendor without clobbering a manual name.
func (s *Server) applyResolve(mac string, res naming.Result) {
	p := store.Patch{}
	if res.Vendor != "" {
		p.Vendor = &res.Vendor
	}
	dev, ok, _ := s.store.Get(mac)
	if res.Name != "" && (!ok || dev.NameSource != "manual") {
		src := res.Source
		p.Name = &res.Name
		p.NameSource = &src
	}
	s.store.Patch(mac, p)
}

// resolveAsync resolves a freshly-tracked device in the background (user-
// initiated, so the active traffic is allowed).
func (s *Server) resolveAsync(mac, ip string) {
	res := naming.Resolve(ip, mac, s.lanResolver(), resolveTimeout)
	s.applyResolve(mac, res)
	s.pushPresence()
}
