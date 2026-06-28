// Package server wires the HTTP surface: the embedded UI, the SSE stream (which
// doubles as the activity gate), and the JSON API. It owns the sampler loop that
// pushes live frames to connected clients.
package server

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zw00sh/puck/puckview/internal/config"
	"github.com/zw00sh/puck/puckview/internal/netconf"
	"github.com/zw00sh/puck/puckview/internal/store"
	"github.com/zw00sh/puck/puckview/internal/sysinfo"
	"github.com/zw00sh/puck/puckview/internal/tailscale"
	"github.com/zw00sh/puck/puckview/internal/wol"
	"github.com/zw00sh/puck/puckview/web"
)

type Server struct {
	cfg     config.Config
	hub     *Hub
	sys     *sysinfo.Sampler
	store   *store.Store
	ni      netconf.Info
	version string
	mux     *http.ServeMux

	waker wol.Waker

	liveMu      sync.Mutex
	liveResults map[string]livePart

	wakeMu sync.Mutex
	wakes  map[string]context.CancelFunc

	scanMu   sync.Mutex
	scanning bool

	// Single-flight guards: probing and meta refresh run off the tick so a slow
	// probe timeout or tailscale/docker shell-out never stalls the 1s stats push.
	probing        atomic.Bool
	metaRefreshing atomic.Bool

	metaMu   sync.Mutex
	tsInfo   *tailscale.Info
	services []serviceView

	nameMu    sync.Mutex
	nameCache map[string]nameEntry

	seenMu    sync.Mutex
	seenWrite map[string]int64 // mac → unix ts of last persisted sighting (throttle)

	obsMu    sync.Mutex
	obsWrite map[string]int64 // mac → unix ts of last observed-table upsert (throttle)
}

func New(cfg config.Config, ni netconf.Info, st *store.Store, version string) *Server {
	s := &Server{
		cfg:         cfg,
		hub:         NewHub(cfg.Grace),
		sys:         sysinfo.New(ni.Iface),
		store:       st,
		ni:          ni,
		version:     version,
		mux:         http.NewServeMux(),
		waker:       wol.Waker{Iface: ni.Iface, Broadcast: ni.Broadcast},
		liveResults: map[string]livePart{},
		wakes:       map[string]context.CancelFunc{},
		nameCache:   map[string]nameEntry{},
		seenWrite:   map[string]int64{},
		obsWrite:    map[string]int64{},
	}
	s.routes()
	return s
}

func nowUnix() int64 { return time.Now().Unix() }

// goProbeTracked runs probeTracked in the background, skipping if a previous
// cycle is still in flight (down hosts make a cycle outlast the 2s interval).
func (s *Server) goProbeTracked() {
	if !s.probing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.probing.Store(false)
		s.probeTracked()
	}()
}

// goRefreshMeta runs refreshMeta in the background with the same single-flight
// guard (it shells out to tailscale/docker, which can be slow).
func (s *Server) goRefreshMeta() {
	if !s.metaRefreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.metaRefreshing.Store(false)
		s.refreshMeta()
	}()
}

func (s *Server) routes() {
	// Embedded UI assets.
	sub, _ := fs.Sub(web.FS, "assets")
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	s.mux.HandleFunc("/", s.handleIndex)

	s.mux.HandleFunc("/events", s.handleEvents)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	s.mux.HandleFunc("GET /api/info", s.handleInfo)

	// Tracked devices (keyed by MAC).
	s.mux.HandleFunc("GET /api/devices", s.handleListDevices)
	s.mux.HandleFunc("POST /api/devices", s.handleTrackDevice)
	s.mux.HandleFunc("PATCH /api/devices/{mac}", s.handlePatchDevice)
	s.mux.HandleFunc("DELETE /api/devices/{mac}", s.handleUntrackDevice)
	s.mux.HandleFunc("POST /api/devices/{mac}/wake", s.handleWake)
	s.mux.HandleFunc("DELETE /api/devices/{mac}/wake", s.handleCancelWake)
	s.mux.HandleFunc("POST /api/devices/{mac}/resolve", s.handleResolve)
	s.mux.HandleFunc("POST /api/devices/{mac}/probes", s.handleAddProbe)
	s.mux.HandleFunc("DELETE /api/devices/{mac}/probes/{port}", s.handleDeleteProbe)

	// Discovery.
	s.mux.HandleFunc("GET /api/cache", s.handleCache)
	s.mux.HandleFunc("POST /api/scan", s.handleScan)
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := web.FS.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "ui not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.helloPayload())
}

// helloPayload is the static-ish identity sent on connect and via /api/info.
func (s *Server) helloPayload() map[string]any {
	host, _ := os.Hostname()
	return map[string]any{
		"host":    host,
		"version": s.version,
		"net":     s.ni,
	}
}

// statsPayload is the live diagnostics frame: passive box stats plus the cached
// tailnet state (refreshed less often than the 1s stats tick).
type statsPayload struct {
	sysinfo.Stats
	TS *tailscale.Info `json:"ts,omitempty"`
}

// pushOnce samples and broadcasts a single stats frame plus current presence.
func (s *Server) pushOnce() {
	s.hub.Broadcast("stats", statsPayload{Stats: s.sys.Sample(), TS: s.getTS()})
	s.pushPresence()
}

// pushPresence reads the (passive) neighbour cache and broadcasts the tracked
// device list and the ARP cache. Safe to call regardless of the activity gate.
func (s *Server) pushPresence() {
	byMAC, entries := neighSnapshot()
	s.hub.Broadcast("devices", s.buildDevices(byMAC))
	s.hub.Broadcast("cache", s.buildCache(entries))
}

// Run drives the live sampler loop until ctx is cancelled. Stats and presence
// are passive (/proc and kernel-cache reads) so they stream whenever a client is
// connected; active probing is additionally gated on hub.Active().
func (s *Server) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var n int
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if s.hub.Clients() == 0 {
				continue
			}
			n++
			// Active probing (ICMP/TCP) is gated and throttled to every 2s; the
			// passive stats/neigh frames stream every second. Run it off the tick
			// so down-host probe timeouts never stall the stats push.
			if s.hub.Active() && n%2 == 0 {
				s.goProbeTracked()
			}
			// tailnet state + service health refresh every ~5s (off the tick: it
			// shells out to tailscale/docker).
			if n%5 == 1 {
				s.goRefreshMeta()
			}
			// prune expired recently-seen observations once a minute.
			if n%60 == 0 {
				go s.store.PruneObserved(nowUnix() - observedTTL)
			}
			// Resolve names for the current neighbour cache (active rDNS/NetBIOS),
			// gated on a live session and throttled. Cached, so it's a no-op once
			// names are known.
			if s.hub.Active() && n%4 == 0 {
				byMAC, _ := neighSnapshot()
				ips := make([]string, 0, len(byMAC))
				for _, e := range byMAC {
					ips = append(ips, e.IP)
				}
				go s.resolveNames(ips)
			}
			s.pushOnce()
		}
	}
}
