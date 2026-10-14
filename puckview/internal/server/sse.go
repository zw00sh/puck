package server

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// pulseInterval is how often the dashboard is expected to re-assert that a
// human is watching. It must stay comfortably below cfg.Grace so that one
// dropped pulse doesn't close the activity gate. Mirrored in web/assets/app.js.
const pulseInterval = 10 * time.Second

// Hub is the SSE fan-out plus the activity gate.
//
// The gate is deliberately NOT "is a socket open". An open connection proves
// nothing about whether anyone is looking: a `tailscale serve` proxy, a
// suspended phone, or a crashed browser will all hold one open indefinitely —
// which is how a puck ended up probing its LAN around the clock for six weeks
// with nobody watching. So liveness has to be asserted by the page itself:
// the dashboard POSTs /api/pulse while it is visible, and Active() is true only
// while a recent pulse stands.
//
// The two gates are separate on purpose:
//   - Clients()  — are there sockets to write to? Gates BROADCASTING.
//   - Active()   — is a human watching? Gates ACTIVE NETWORK PROBING.
type Hub struct {
	grace time.Duration

	mu      sync.Mutex
	clients map[chan []byte]struct{}

	lastPulse atomic.Int64 // UnixNano of the most recent proof-of-life (0 = never)
}

func NewHub(grace time.Duration) *Hub {
	return &Hub{grace: grace, clients: make(map[chan []byte]struct{})}
}

// Clients returns the current connected client count.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Pulse records proof that a dashboard is visible to someone. Nanosecond
// precision: at second granularity a fresh pulse can already read as up to a
// second old, which matters when the grace window is short.
func (h *Hub) Pulse() { h.lastPulse.Store(time.Now().UnixNano()) }

// Active reports whether active network probing is currently authorized.
func (h *Hub) Active() bool {
	p := h.lastPulse.Load()
	if p == 0 {
		return false
	}
	return time.Since(time.Unix(0, p)) < h.grace
}

// send fans an already-marshalled frame out to every client. Slow clients are
// skipped rather than blocking the render loop.
func (h *Hub) send(msg []byte) {
	if msg == nil {
		return
	}
	h.mu.Lock()
	for ch := range h.clients {
		select {
		case ch <- msg:
		default: // drop for slow client
		}
	}
	h.mu.Unlock()
}

// Broadcast marshals and fans out a named event. Used for the ad-hoc streams
// (wake, scan) that aren't part of the per-tick frame set.
func (h *Hub) Broadcast(event string, data any) { h.send(sseFrame(event, data)) }

func (h *Hub) add() chan []byte {
	ch := make(chan []byte, 16)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) remove(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

// handleEvents is the GET /events SSE endpoint.
//
// Nothing expensive may happen before the headers are flushed. The old version
// built a full presence frame first, which meant the browser saw no response at
// all — not even a status line — until that work finished; a slow frame showed
// up as a dashboard that loaded instantly and then sat empty. Headers go out
// first, then the cached frame set, then we just relay.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Attach before writing the greeting, so a frame published mid-greeting is
	// queued rather than missed. A duplicate frame is harmless — renders idempotently.
	ch := s.hub.add()
	defer s.hub.remove(ch)

	// A connecting client is proof someone is watching; don't make them wait a
	// pulse interval for probing to start.
	s.hub.Pulse()

	greeting := s.frames.Load().greeting()
	if greeting == nil {
		// Before the first render tick (only in the first second after boot).
		greeting = [][]byte{sseFrame("hello", s.helloPayload())}
	}
	for _, b := range greeting {
		if b == nil {
			continue
		}
		if _, err := w.Write(b); err != nil {
			return
		}
	}
	flusher.Flush()

	// Heartbeat keeps proxies from idling the connection out.
	hb := time.NewTicker(20 * time.Second)
	defer hb.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		case <-hb.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handlePulse is POST /api/pulse — the dashboard's proof-of-life, which gates
// active probing. See Hub.
func (s *Server) handlePulse(w http.ResponseWriter, r *http.Request) {
	s.hub.Pulse()
	w.WriteHeader(http.StatusNoContent)
}
