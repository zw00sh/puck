package server

import (
	"path/filepath"
	"testing"

	"github.com/zw00sh/puck/puckview/internal/neigh"
	"github.com/zw00sh/puck/puckview/internal/store"
)

func newTestState(t *testing.T, devs ...store.Device) (*state, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, d := range devs {
		if err := st.Add(d); err != nil {
			t.Fatal(err)
		}
	}
	s := newState()
	if err := s.Load(st); err != nil {
		t.Fatal(err)
	}
	return s, st
}

func TestApplyProbesAdvancesScalars(t *testing.T) {
	mac := "aa:bb:cc:dd:ee:01"
	s, _ := newTestState(t, store.Device{MAC: mac, IP: "192.0.2.5"})

	up := map[string]livePart{mac: {icmp: &icmpResult{St: "up", RTT: 3}}}
	s.applyProbes(up, 1000)

	ds := s.snapshot()[0]
	if !ds.icmpSeen {
		t.Error("icmpSeen not set after an ICMP reply")
	}
	if ds.lastSeenUp != 1000 || ds.lastSeen != 1000 {
		t.Errorf("lastSeenUp=%d lastSeen=%d, want 1000/1000", ds.lastSeenUp, ds.lastSeen)
	}
	if !ds.dirty {
		t.Error("state not marked dirty, so the scalars would never be persisted")
	}

	// A TCP-only reply counts as up but must not claim ICMP has ever answered.
	mac2 := "aa:bb:cc:dd:ee:02"
	s2, _ := newTestState(t, store.Device{MAC: mac2, IP: "192.0.2.6"})
	s2.applyProbes(map[string]livePart{mac2: {
		icmp:   &icmpResult{St: "down"},
		probes: []probeView{{Port: 3389, St: "up"}},
	}}, 2000)
	ds2 := s2.snapshot()[0]
	if ds2.icmpSeen {
		t.Error("icmpSeen set by a TCP-only reply")
	}
	if ds2.lastSeenUp != 2000 {
		t.Errorf("lastSeenUp = %d, want 2000", ds2.lastSeenUp)
	}
}

// The transition log is the whole reason the history table stays small: a row
// per state CHANGE, never one per probe cycle.
func TestTransitionsLoggedOnlyOnChange(t *testing.T) {
	mac := "aa:bb:cc:dd:ee:01"
	s, _ := newTestState(t, store.Device{MAC: mac, IP: "192.0.2.5"})
	up := map[string]livePart{mac: {icmp: &icmpResult{St: "up"}}}
	down := map[string]livePart{mac: {icmp: &icmpResult{St: "down"}}}

	s.applyProbes(up, 1000) // first verdict after a restart: not a transition
	s.applyProbes(up, 1002) // steady state
	s.applyProbes(up, 1004)
	if got := len(s.drain().Events); got != 0 {
		t.Fatalf("%d events for a steady-up device, want 0", got)
	}

	s.applyProbes(down, 1006) // up → down
	s.applyProbes(down, 1008) // steady
	s.applyProbes(up, 1010)   // down → up
	events := s.drain().Events
	if len(events) != 2 {
		t.Fatalf("%d events across two transitions, want 2: %+v", len(events), events)
	}
	if events[0].State != "down" || events[0].Signal != "none" || events[0].TS != 1006 {
		t.Errorf("first event = %+v, want down/none at 1006", events[0])
	}
	if events[1].State != "up" || events[1].Signal != "icmp" || events[1].TS != 1010 {
		t.Errorf("second event = %+v, want up/icmp at 1010", events[1])
	}
}

func TestApplyNeighTracksPresenceAndIPMoves(t *testing.T) {
	mac := "aa:bb:cc:dd:ee:01"
	s, _ := newTestState(t, store.Device{MAC: mac, IP: "192.0.2.5"})

	s.applyNeigh(map[string]neigh.Entry{
		mac: {MAC: mac, IP: "192.0.2.7", State: "up", SeenSec: 4},
	}, 3000)

	ds := s.snapshot()[0]
	if ds.arp != "up" || ds.arpSeen != 4 {
		t.Errorf("arp=%s arpSeen=%d, want up/4", ds.arp, ds.arpSeen)
	}
	if ds.lastSeen != 3000 {
		t.Errorf("lastSeen = %d, want 3000", ds.lastSeen)
	}
	// ARP presence is not liveness, so it must not move lastSeenUp.
	if ds.lastSeenUp != 0 {
		t.Errorf("lastSeenUp = %d; ARP presence must not count as liveness", ds.lastSeenUp)
	}
	if ds.dev.IP != "192.0.2.7" {
		t.Errorf("IP = %s, want the ARP-observed 192.0.2.7", ds.dev.IP)
	}
	b := s.drain()
	if len(b.IPMoves) != 1 || b.IPMoves[0].IP != "192.0.2.7" {
		t.Errorf("IPMoves = %+v, want one move to 192.0.2.7", b.IPMoves)
	}

	// Dropping out of the neigh cache reads as down with an unknown ARP age.
	s.applyNeigh(map[string]neigh.Entry{}, 3100)
	ds = s.snapshot()[0]
	if ds.arp != "down" || ds.arpSeen != -1 {
		t.Errorf("arp=%s arpSeen=%d after dropping out, want down/-1", ds.arp, ds.arpSeen)
	}
}

func TestDrainAndRestore(t *testing.T) {
	mac := "aa:bb:cc:dd:ee:01"
	s, _ := newTestState(t, store.Device{MAC: mac, IP: "192.0.2.5"})
	s.applyProbes(map[string]livePart{mac: {icmp: &icmpResult{St: "up"}}}, 1000)
	s.applyProbes(map[string]livePart{mac: {icmp: &icmpResult{St: "down"}}}, 1002)

	b := s.drain()
	if len(b.Seen) != 1 || len(b.Events) != 1 {
		t.Fatalf("drain = %d seen / %d events, want 1/1", len(b.Seen), len(b.Events))
	}
	if next := s.drain(); !next.Empty() {
		t.Errorf("second drain not empty: %+v", next)
	}

	// A failed flush must not silently drop history.
	s.restore(b)
	again := s.drain()
	if len(again.Seen) != 1 || len(again.Events) != 1 {
		t.Errorf("after restore, drain = %d seen / %d events, want 1/1", len(again.Seen), len(again.Events))
	}
}

// Load must never regress presence that memory holds but the store hasn't seen
// yet — a reload races the write-behind flush on every mutation.
func TestLoadKeepsAheadOfStoreValues(t *testing.T) {
	mac := "aa:bb:cc:dd:ee:01"
	s, st := newTestState(t, store.Device{MAC: mac, IP: "192.0.2.5"})
	s.applyProbes(map[string]livePart{mac: {icmp: &icmpResult{St: "up"}}}, 5000)

	if err := s.Load(st); err != nil { // store still has zeroes for these
		t.Fatal(err)
	}
	ds := s.snapshot()[0]
	if !ds.icmpSeen || ds.lastSeenUp != 5000 {
		t.Errorf("reload regressed memory: icmpSeen=%v lastSeenUp=%d, want true/5000",
			ds.icmpSeen, ds.lastSeenUp)
	}
}

func TestSeenStateSemantics(t *testing.T) {
	now := int64(10_000)
	cases := []struct {
		name     string
		ds       devState
		v        deviceView
		wantSeen string
		wantL2   bool
	}{{
		name:     "up now",
		ds:       devState{},
		v:        deviceView{Icmp: &icmpResult{St: "up"}},
		wantSeen: "now",
	}, {
		name:     "down with a confirmed sighting",
		ds:       devState{lastSeenUp: now - 30},
		v:        deviceView{Icmp: &icmpResult{St: "down"}},
		wantSeen: neigh.FmtSeen(30),
	}, {
		// Nothing is expected of a bare host, so ARP presence is shown but marked.
		name:     "never up, pure presence device falls back to L2",
		ds:       devState{arpSeen: 12, dev: store.Device{CreatedAt: now - 100}},
		v:        deviceView{Icmp: &icmpResult{St: "down"}},
		wantSeen: neigh.FmtSeen(12),
		wantL2:   true,
	}, {
		// This device has answered ICMP before, so an ARP-only showing is suspect:
		// presence must not be dressed up as a sighting.
		name:     "never up but icmpSeen shows no fallback",
		ds:       devState{arpSeen: 12, icmpSeen: true, dev: store.Device{CreatedAt: now - 100}},
		v:        deviceView{Icmp: &icmpResult{St: "down"}},
		wantSeen: "—",
	}, {
		name: "never up but has watchdogs shows no fallback",
		ds: devState{arpSeen: 12, dev: store.Device{
			CreatedAt: now - 100, Probes: []int{3389},
		}},
		v:        deviceView{Icmp: &icmpResult{St: "down"}},
		wantSeen: "—",
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seen, l2, _ := seenState(c.ds, c.v, now)
			if seen != c.wantSeen || l2 != c.wantL2 {
				t.Errorf("seenState = (%q, %v), want (%q, %v)", seen, l2, c.wantSeen, c.wantL2)
			}
		})
	}
}
