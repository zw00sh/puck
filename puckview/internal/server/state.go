package server

import (
	"sort"
	"sync"

	"github.com/zw00sh/puck/puckview/internal/neigh"
	"github.com/zw00sh/puck/puckview/internal/store"
)

// This file holds the server's authoritative view of presence.
//
// SQLite is the journal, not the read path. Device CONFIG (name, probes, IP,
// notes) is still owned by the store and reloaded here after any mutation;
// device PRESENCE (has it ever answered ICMP, when was it last up, when was it
// last seen at all) is owned by this struct and flushed back through
// store.Flush on a slow cadence. Nothing on the per-second render path touches
// the database.
//
// The accepted trade-off: an unclean kill loses up to flushInterval of
// last-seen movement. That is immaterial for a presence dashboard, and it buys
// a render path with no I/O in it at all.

// devState is the in-memory record for one tracked device.
type devState struct {
	dev store.Device // config, as loaded from the store

	// Persistent presence scalars (memory is the authority between flushes).
	icmpSeen   bool
	lastSeenUp int64
	lastSeen   int64

	// Session-local observations, never persisted.
	live    livePart
	arp     string
	arpSeen int64 // kernel's "confirmed" age in seconds; -1 when unknown

	// status is the last up/down verdict, used to spot transitions worth logging.
	status string

	dirty bool // presence scalars have moved since the last flush
}

// state is the whole in-memory model: tracked devices plus the untracked hosts
// seen on the LAN.
type state struct {
	mu    sync.RWMutex
	devs  map[string]*devState
	order []string // render order, as the store returned it

	obs      map[string]store.Observed
	obsDirty map[string]bool
	// obsFlushed is the last-persisted sighting per host, so a host that is
	// present every second still only costs a write a minute. The view reads the
	// in-memory value; the stored one only has to survive a restart.
	obsFlushed map[string]int64

	ipMoves map[string]string // mac → new IP, pending persistence
	events  []store.SeenEvent // pending transition rows
}

func newState() *state {
	return &state{
		devs:       map[string]*devState{},
		obs:        map[string]store.Observed{},
		obsDirty:   map[string]bool{},
		obsFlushed: map[string]int64{},
		ipMoves:    map[string]string{},
	}
}

// Load (re)reads device config from the store, preserving everything memory is
// authoritative for. Called at startup and after a config mutation — never on
// the render path.
func (s *state) Load(st *store.Store) error {
	devs, err := st.List()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	order := make([]string, 0, len(devs))
	next := make(map[string]*devState, len(devs))
	for _, d := range devs {
		order = append(order, d.MAC)
		ds, ok := s.devs[d.MAC]
		if !ok {
			// First sight of this device: seed presence from the stored scalars.
			next[d.MAC] = &devState{
				dev: d, icmpSeen: d.IcmpSeen, lastSeenUp: d.LastSeenUp,
				lastSeen: d.LastSeen, arp: "down", arpSeen: -1,
			}
			continue
		}
		// Keep the live view; take whichever scalars are further ahead, since a
		// reload races an in-flight flush and these values only move forward.
		ds.dev = d
		ds.icmpSeen = ds.icmpSeen || d.IcmpSeen
		ds.lastSeenUp = max64(ds.lastSeenUp, d.LastSeenUp)
		ds.lastSeen = max64(ds.lastSeen, d.LastSeen)
		next[d.MAC] = ds
	}
	s.devs, s.order = next, order
	return nil
}

// LoadObserved seeds the untracked-host view from the store.
func (s *state) LoadObserved(st *store.Store, sinceTS int64) error {
	obs, err := st.ListObserved(sinceTS)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range obs {
		if cur, ok := s.obs[o.MAC]; !ok || o.LastSeen > cur.LastSeen {
			s.obs[o.MAC] = o
		}
	}
	return nil
}

// applyNeigh folds a kernel neighbour snapshot into the model: ARP state, the
// last-seen clock, and any IP move. Passive, so it runs regardless of the gate.
func (s *state) applyNeigh(byMAC map[string]neigh.Entry, now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for mac, ds := range s.devs {
		e, present := byMAC[mac]
		if !present {
			ds.arp, ds.arpSeen = "down", -1
			continue
		}
		ds.arp, ds.arpSeen = e.State, e.SeenSec
		// Presence is a sighting (though not liveness — see seenState).
		if now > ds.lastSeen {
			ds.lastSeen = now
			ds.dirty = true
		}
		if e.IP != "" && e.IP != ds.dev.IP {
			ds.dev.IP = e.IP // ARP reconciliation: the device moved IPs
			s.ipMoves[mac] = e.IP
		}
	}
}

// applyProbes folds a probe cycle's results into the model, advancing the
// liveness scalars and recording any up/down transition.
func (s *state) applyProbes(results map[string]livePart, now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for mac, part := range results {
		ds, ok := s.devs[mac]
		if !ok {
			continue // untracked mid-cycle
		}
		ds.live = part

		signal := ""
		if part.icmp != nil && part.icmp.St == "up" {
			signal = "icmp"
			if !ds.icmpSeen {
				ds.icmpSeen = true
				ds.dirty = true
			}
		} else {
			for _, pv := range part.probes {
				if pv.St == "up" {
					signal = "tcp"
					break
				}
			}
		}
		status := "down"
		if signal != "" {
			status = "up"
			ds.lastSeenUp, ds.lastSeen = now, now
			ds.dirty = true
		}
		// Log transitions only — never the first verdict after a restart, and
		// never the steady state. This is what keeps the history table small.
		if ds.status != "" && ds.status != status {
			sig := signal
			if sig == "" {
				sig = "none"
			}
			s.events = append(s.events, store.SeenEvent{MAC: mac, TS: now, State: status, Signal: sig})
		}
		ds.status = status
	}
}

// clearLive drops cached probe results, so a device shows no stale liveness once
// the activity gate has closed and probing stopped.
func (s *state) clearLive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ds := range s.devs {
		ds.live = livePart{}
		ds.status = ""
	}
}

// snapshot returns the tracked devices in render order. The returned slice is a
// copy, so the caller can build JSON without holding the lock.
func (s *state) snapshot() []devState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]devState, 0, len(s.order))
	for _, mac := range s.order {
		if ds, ok := s.devs[mac]; ok {
			out = append(out, *ds)
		}
	}
	return out
}

// tracked reports the set of tracked MACs.
func (s *state) tracked() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]bool, len(s.devs))
	for mac := range s.devs {
		out[mac] = true
	}
	return out
}

// touchObserved records a sighting of an untracked host in memory.
func (s *state) touchObserved(mac, ip, name string, now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.obs[mac]
	cur.MAC, cur.IP, cur.LastSeen = mac, ip, now
	if name != "" {
		cur.Name = name
	}
	s.obs[mac] = cur
	if now-s.obsFlushed[mac] >= 60 {
		s.obsDirty[mac] = true
	}
}

// observedSince returns remembered untracked hosts seen at or after sinceTS,
// newest first. The order must be deterministic: the render loop decides whether
// to broadcast by comparing marshalled frames, so map iteration order would make
// every frame look different and defeat the comparison.
func (s *state) observedSince(sinceTS int64) []store.Observed {
	s.mu.RLock()
	out := make([]store.Observed, 0, len(s.obs))
	for _, o := range s.obs {
		if o.LastSeen >= sinceTS {
			out = append(out, o)
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen != out[j].LastSeen {
			return out[i].LastSeen > out[j].LastSeen
		}
		return out[i].MAC < out[j].MAC // stable tiebreak
	})
	return out
}

// forgetObserved drops a host from the untracked view (e.g. once it's tracked).
func (s *state) forgetObserved(mac string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.obs, mac)
	delete(s.obsDirty, mac)
	delete(s.obsFlushed, mac)
}

// pruneObserved evicts remembered hosts older than beforeTS from memory.
func (s *state) pruneObserved(beforeTS int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for mac, o := range s.obs {
		if o.LastSeen < beforeTS {
			delete(s.obs, mac)
			delete(s.obsDirty, mac)
			delete(s.obsFlushed, mac)
		}
	}
}

// drain collects everything awaiting persistence and clears the pending marks.
// Scalars are written with MAX() guards on the store side, so losing a race with
// a concurrent reload cannot move a last-seen backwards.
func (s *state) drain() store.Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b store.Batch
	for mac, ds := range s.devs {
		if !ds.dirty {
			continue
		}
		b.Seen = append(b.Seen, store.SeenUpdate{
			MAC: mac, IcmpSeen: ds.icmpSeen,
			LastSeenUp: ds.lastSeenUp, LastSeen: ds.lastSeen,
		})
		ds.dirty = false
	}
	for mac := range s.obsDirty {
		if o, ok := s.obs[mac]; ok {
			b.Obs = append(b.Obs, store.ObservedUpdate{MAC: o.MAC, IP: o.IP, Name: o.Name, LastSeen: o.LastSeen})
			s.obsFlushed[mac] = o.LastSeen
		}
	}
	s.obsDirty = map[string]bool{}
	for mac, ip := range s.ipMoves {
		b.IPMoves = append(b.IPMoves, store.IPMove{MAC: mac, IP: ip})
	}
	s.ipMoves = map[string]string{}
	b.Events, s.events = s.events, nil
	return b
}

// restore hands a failed batch back to the model so the next flush retries it.
// Scalars just get their dirty mark back (memory still holds the values, and the
// store MAX()-guards them); the transition rows have to be re-queued explicitly,
// since memory has no other copy of them.
func (s *state) restore(b store.Batch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range b.Seen {
		if ds, ok := s.devs[u.MAC]; ok {
			ds.dirty = true
		}
	}
	for _, o := range b.Obs {
		if _, ok := s.obs[o.MAC]; ok {
			s.obsDirty[o.MAC] = true
		}
	}
	for _, m := range b.IPMoves {
		if _, pending := s.ipMoves[m.MAC]; !pending {
			s.ipMoves[m.MAC] = m.IP
		}
	}
	s.events = append(b.Events, s.events...)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
