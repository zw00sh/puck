// Package server wires the HTTP surface: the embedded UI, the SSE stream, and
// the JSON API. It owns the loops that keep the live frame set fresh.
//
// Four cadences run independently, each with its own gate, rather than being
// multiplexed onto one counter:
//
//	render       1s   ungated    passive /proc + netlink reads → publish frames
//	probe        2s   Active()   ICMP/TCP — real packets, needs a watcher
//	names        4s   Active()   rDNS/NetBIOS — real queries, needs a watcher
//	meta         5s   Clients()  tailscale/docker shell-outs
//	maintenance 30s   ungated    write-behind flush; hourly retention prune
//
// They used to share one ticker with `n%2`/`n%5`/`n%60` arms, which meant the
// intervals were "every N frames" rather than N seconds — when the render loop
// slowed down, the once-a-minute prune quietly became once every few minutes —
// and that maintenance could not run at all while nobody was connected.
package server

import (
	"context"
	"io/fs"
	"log"
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

const (
	renderInterval = time.Second
	probeInterval  = 2 * time.Second
	namesInterval  = 4 * time.Second
	metaInterval   = 5 * time.Second

	// flushInterval bounds how much last-seen movement an unclean kill can lose.
	flushInterval = 30 * time.Second
	pruneInterval = time.Hour

	// seenEventTTL is how long up/down transitions are kept. Transitions are rare
	// (a row per state change, not per probe), so this window stays small on disk.
	seenEventTTL = 90 * 24 * 60 * 60 // 90 days, seconds
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

	// state is the authority for presence; the store is its journal.
	state *state

	// frames is the latest published frame set — what a connecting client gets.
	frames atomic.Pointer[frameSet]
	// kick asks the render loop for an immediate frame after a mutation.
	kick chan struct{}

	wakeMu sync.Mutex
	wakes  map[string]context.CancelFunc

	scanMu   sync.Mutex
	scanning bool

	metaMu   sync.Mutex
	tsInfo   *tailscale.Info
	services []serviceView

	nameMu    sync.Mutex
	nameCache map[string]nameEntry
}

func New(cfg config.Config, ni netconf.Info, st *store.Store, version string) *Server {
	s := &Server{
		cfg:       cfg,
		hub:       NewHub(cfg.Grace),
		sys:       sysinfo.New(ni.Iface),
		store:     st,
		ni:        ni,
		version:   version,
		mux:       http.NewServeMux(),
		waker:     wol.Waker{Iface: ni.Iface, Broadcast: ni.Broadcast},
		state:     newState(),
		kick:      make(chan struct{}, 1),
		wakes:     map[string]context.CancelFunc{},
		nameCache: map[string]nameEntry{},
	}
	s.routes()
	return s
}

func nowUnix() int64 { return time.Now().Unix() }

func (s *Server) routes() {
	// Embedded UI assets.
	sub, _ := fs.Sub(web.FS, "assets")
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	s.mux.HandleFunc("/", s.handleIndex)

	s.mux.HandleFunc("/events", s.handleEvents)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	s.mux.HandleFunc("GET /api/info", s.handleInfo)
	s.mux.HandleFunc("POST /api/pulse", s.handlePulse)

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

// reloadState re-reads device config from the store into memory. Called after a
// mutation, never on the render path.
func (s *Server) reloadState() {
	if err := s.state.Load(s.store); err != nil {
		log.Printf("reload devices: %v", err)
	}
}

// kickRender asks for an immediate frame rebuild (non-blocking; a pending kick
// is enough). Mutating handlers use this instead of rendering inline, so a click
// returns as soon as it is persisted.
func (s *Server) kickRender() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// render samples the box, folds the kernel neighbour cache into the model, and
// publishes a fresh frame set. Broadcasting is skipped when nobody is attached,
// but publishing is not: a connecting client is served the published frames
// directly, so they must always be current.
func (s *Server) render() {
	byMAC, entries := neighSnapshot()
	s.state.applyNeigh(byMAC, nowUnix())

	fs := &frameSet{
		hello:    sseFrame("hello", s.helloPayload()),
		stats:    sseFrame("stats", statsPayload{Stats: s.sys.Sample(), TS: s.getTS()}),
		devices:  sseFrame("devices", s.buildDevices()),
		cache:    sseFrame("cache", s.buildCache(entries)),
		services: sseFrame("services", s.snapshotServices()),
	}
	prev := s.frames.Swap(fs)
	if s.hub.Clients() == 0 {
		return
	}
	if prev == nil {
		prev = &frameSet{}
	}
	// Stats is the dashboard's clock, so it always goes. The rest only move when
	// they actually differ, which keeps the client from re-rendering tables that
	// haven't changed.
	s.hub.send(fs.stats)
	if changed(prev.devices, fs.devices) {
		s.hub.send(fs.devices)
	}
	if changed(prev.cache, fs.cache) {
		s.hub.send(fs.cache)
	}
	if changed(prev.services, fs.services) {
		s.hub.send(fs.services)
	}
}

// Run drives the live loops until ctx is cancelled, then flushes once more.
func (s *Server) Run(ctx context.Context) {
	if s.cfg.Grace <= pulseInterval {
		log.Printf("warning: PUCKVIEW_GRACE (%s) is not above the dashboard's %s pulse interval; "+
			"the activity gate will flap and probing will stutter", s.cfg.Grace, pulseInterval)
	}
	s.reloadState()
	if err := s.state.LoadObserved(s.store, nowUnix()-observedTTL); err != nil {
		log.Printf("load observed hosts: %v", err)
	}
	go s.backfillLegacy()

	s.refreshMeta() // warm the tailnet/services cache before the first frame
	s.render()      // publish immediately so an early connect isn't empty

	var wg sync.WaitGroup
	for _, loop := range []func(context.Context){
		s.renderLoop, s.probeLoop, s.namesLoop, s.metaLoop, s.maintenanceLoop,
	} {
		wg.Add(1)
		go func(f func(context.Context)) {
			defer wg.Done()
			f(ctx)
		}(loop)
	}
	wg.Wait()

	// Every loop has stopped, so nothing can dirty state behind this final flush.
	s.flush()
}

// renderLoop republishes the frame set every second. Deliberately ungated: the
// reads are passive, and a stale published frame would hand a connecting client
// an out-of-date clock — the cost of keeping it fresh is a few hundred
// microseconds of /proc parsing.
func (s *Server) renderLoop(ctx context.Context) {
	tick := time.NewTicker(renderInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.render()
		case <-s.kick:
			s.render()
		}
	}
}

// probeLoop runs the active ICMP/TCP sweep while a watcher is present. The loop
// is serial, so a cycle that outlasts the interval simply delays the next one.
func (s *Server) probeLoop(ctx context.Context) {
	tick := time.NewTicker(probeInterval)
	defer tick.Stop()
	probed := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !s.hub.Active() {
				// Gate closed: drop cached results so the UI stops reporting a
				// liveness nobody is still checking.
				if probed {
					s.state.clearLive()
					probed = false
				}
				continue
			}
			probed = true
			s.probeTracked()
		}
	}
}

// namesLoop resolves names for the current neighbour cache (active rDNS and
// NetBIOS queries). Cached with a TTL, so it is a no-op once names are known.
func (s *Server) namesLoop(ctx context.Context) {
	tick := time.NewTicker(namesInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !s.hub.Active() {
				continue
			}
			byMAC, _ := neighSnapshot()
			ips := make([]string, 0, len(byMAC))
			for _, e := range byMAC {
				ips = append(ips, e.IP)
			}
			s.resolveNames(ips)
		}
	}
}

// metaLoop refreshes the tailnet state and service health. Gated on attached
// clients rather than on a pulse: it shells out but puts nothing on the LAN.
func (s *Server) metaLoop(ctx context.Context) {
	tick := time.NewTicker(metaInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if s.hub.Clients() == 0 {
				continue
			}
			s.refreshMeta()
		}
	}
}

// maintenanceLoop owns durability and retention. Ungated, so housekeeping still
// happens on a box nobody has looked at for a month.
func (s *Server) maintenanceLoop(ctx context.Context) {
	flush := time.NewTicker(flushInterval)
	defer flush.Stop()
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			s.flush()
		case <-prune.C:
			s.prune()
		}
	}
}

// flush writes accumulated in-memory changes back to the store. On failure the
// batch is handed back so the next flush retries it rather than losing history.
func (s *Server) flush() {
	b := s.state.drain()
	if b.Empty() {
		return
	}
	if err := s.store.Flush(b); err != nil {
		log.Printf("flush presence: %v (will retry)", err)
		s.state.restore(b)
	}
}

func (s *Server) prune() {
	cutoff := nowUnix() - observedTTL
	s.state.pruneObserved(cutoff)
	if err := s.store.PruneObserved(cutoff); err != nil {
		log.Printf("prune observed: %v", err)
	}
	if err := s.store.PruneSeenEvents(nowUnix() - seenEventTTL); err != nil {
		log.Printf("prune seen events: %v", err)
	}
}

// backfillLegacy collapses a pre-v1 seen_history log into the per-device derived
// columns. It runs in the background because proving that a device has NEVER
// answered ICMP costs a scan of that device's rows, and a fleet box can carry
// over a million of them; blocking startup on that would make the dashboard
// unreachable for the duration. Until it finishes, devices simply read as
// never-seen, which the UI already renders as "—".
func (s *Server) backfillLegacy() {
	if !s.store.LegacyBackfillPending() {
		return
	}
	log.Print("presence migration: collapsing the legacy seen_history log into derived columns")
	start := time.Now()
	n, freed, err := s.store.BackfillLegacy()
	if err != nil {
		log.Printf("presence migration failed, will retry on next start: %v", err)
		return
	}
	log.Printf("presence migration done: %d device(s) in %s, reclaimed %.1f MB",
		n, time.Since(start).Round(time.Millisecond), float64(freed)/1e6)
	s.reloadState() // pick up the backfilled scalars
}
