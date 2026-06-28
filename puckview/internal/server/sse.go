package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Hub is the SSE fan-out and the activity gate in one. An open client
// connection is what authorizes active probing; when the last client leaves,
// Active() stays true for the grace window then flips false so the sampler goes
// quiet (the "good network citizen" requirement).
type Hub struct {
	grace time.Duration

	mu          sync.Mutex
	clients     map[chan []byte]struct{}
	lastSeenAt  time.Time // when client count last dropped to zero
	everHadDrop bool
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

// Active reports whether active network probing is currently authorized.
func (h *Hub) Active() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) > 0 {
		return true
	}
	if !h.everHadDrop {
		return false
	}
	return time.Since(h.lastSeenAt) < h.grace
}

// Broadcast marshals data and sends it to all connected clients as a named SSE
// event. Slow clients are skipped rather than blocking the sampler.
func (h *Hub) Broadcast(event string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, payload))
	h.mu.Lock()
	for ch := range h.clients {
		select {
		case ch <- msg:
		default: // drop for slow client
		}
	}
	h.mu.Unlock()
}

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
	if len(h.clients) == 0 {
		h.lastSeenAt = time.Now()
		h.everHadDrop = true
	}
	h.mu.Unlock()
	close(ch)
}

// handleEvents is the GET /events SSE endpoint.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.hub.add()
	defer s.hub.remove(ch)

	// Greet immediately so the client can render the static identity, and push a
	// first stats frame without waiting for the next tick.
	s.hub.Broadcast("hello", s.helloPayload())
	s.pushOnce()
	if svcs := s.snapshotServices(); svcs != nil {
		s.hub.Broadcast("services", svcs)
	} else {
		s.goRefreshMeta() // first client: warm tailnet/services cache
	}

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
