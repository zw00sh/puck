package server

import (
	"testing"
	"time"
)

// The activity gate is the reason a puck stopped probing its LAN around the
// clock: it must key off a dashboard's proof-of-life, not off an open socket.
func TestActiveRequiresAPulse(t *testing.T) {
	h := NewHub(50 * time.Millisecond)

	if h.Active() {
		t.Error("Active() true before any pulse")
	}

	// An attached client is NOT on its own enough — a proxy or a suspended tab
	// holds a socket open indefinitely.
	ch := h.add()
	defer h.remove(ch)
	if h.Clients() != 1 {
		t.Fatalf("Clients() = %d, want 1", h.Clients())
	}
	if h.Active() {
		t.Error("Active() true for an attached-but-silent client")
	}

	h.Pulse()
	if !h.Active() {
		t.Error("Active() false immediately after a pulse")
	}

	// The gate closes once the pulse goes stale, client or no client.
	time.Sleep(80 * time.Millisecond)
	if h.Active() {
		t.Error("Active() still true after the grace window expired")
	}
}

func TestSendSkipsSlowClientsAndDropsNil(t *testing.T) {
	h := NewHub(time.Second)
	ch := h.add()
	defer h.remove(ch)

	h.send(nil) // an unmarshalable frame must not enqueue anything
	select {
	case msg := <-ch:
		t.Fatalf("nil frame delivered: %q", msg)
	default:
	}

	h.send([]byte("event: x\ndata: 1\n\n"))
	select {
	case msg := <-ch:
		if string(msg) != "event: x\ndata: 1\n\n" {
			t.Errorf("got %q", msg)
		}
	default:
		t.Fatal("frame not delivered")
	}

	// Overfill the buffer; send must drop rather than block the render loop.
	for i := 0; i < 100; i++ {
		h.send([]byte("event: y\ndata: 1\n\n"))
	}
}

func TestFrameSetGreetingAndChangeDetection(t *testing.T) {
	var nilSet *frameSet
	if got := nilSet.greeting(); got != nil {
		t.Errorf("nil frameSet greeting = %v, want nil", got)
	}

	fs := &frameSet{
		hello:   sseFrame("hello", map[string]string{"host": "puck"}),
		stats:   sseFrame("stats", map[string]int{"up": 1}),
		devices: sseFrame("devices", []string{}),
	}
	g := fs.greeting()
	if len(g) != 5 {
		t.Fatalf("greeting has %d slots, want 5", len(g))
	}
	if string(g[0]) != "event: hello\ndata: {\"host\":\"puck\"}\n\n" {
		t.Errorf("hello frame = %q", g[0])
	}

	if changed(fs.devices, fs.devices) {
		t.Error("identical frames reported as changed")
	}
	if !changed(fs.devices, fs.stats) {
		t.Error("differing frames reported as unchanged")
	}
	if changed(fs.devices, nil) {
		t.Error("a nil new frame must not count as a change")
	}
}
