package server

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/zw00sh/puck/puckview/internal/store"
)

const (
	wakeTimeout  = 120 * time.Second
	wakeResendEv = 2 // resend the barrage every N seconds while waiting
)

// wakeEvent is the per-tick payload streamed on the SSE `wake` channel. It never
// claims success it can't observe: `up` flips only when ARP confirms the host.
type wakeEvent struct {
	MAC     string      `json:"mac"`
	Elapsed int         `json:"elapsed"`
	Timeout int         `json:"timeout"` // seconds; drives the dialog progress bar
	Packets int         `json:"packets"`
	Arp     string      `json:"arp"`
	Icmp    *icmpResult `json:"icmp"`
	Probes  []probeView `json:"probes"`
	Up      bool        `json:"up"`
	Done    bool        `json:"done"`
}

// POST /api/devices/{mac}/wake → 202; the wake-and-watch runs in the background
// and reports progress over SSE.
func (s *Server) handleWake(w http.ResponseWriter, r *http.Request) {
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
	hw, err := net.ParseMAC(dev.MAC)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad mac")
		return
	}

	// Cancel any in-flight wake for this MAC and start fresh.
	s.wakeMu.Lock()
	if cancel, ok := s.wakes[mac]; ok {
		cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.wakes[mac] = cancel
	s.wakeMu.Unlock()

	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"status":"sent"}`))

	go s.runWake(ctx, dev, hw)
}

// DELETE /api/devices/{mac}/wake → cancel an in-flight wake (e.g. the user
// closed the wake dialog). Stops the packet barrage immediately.
func (s *Server) handleCancelWake(w http.ResponseWriter, r *http.Request) {
	mac := store.NormMAC(r.PathValue("mac"))
	s.wakeMu.Lock()
	if cancel, ok := s.wakes[mac]; ok {
		cancel()
	}
	s.wakeMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) runWake(ctx context.Context, dev store.Device, hw net.HardwareAddr) {
	defer func() {
		s.wakeMu.Lock()
		delete(s.wakes, store.NormMAC(dev.MAC))
		s.wakeMu.Unlock()
	}()

	start := time.Now()
	packets := 0
	send := func() {
		n, _ := s.waker.Send(hw, dev.IP)
		packets += n
	}
	send() // initial barrage

	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		elapsed := int(time.Since(start).Seconds())

		// Observe all signals. Probe ICMP and every watchdog concurrently — a down
		// host (the whole point of a wake) times out each probe at probeTimeout, so
		// sequential probing would stretch the loop well past the 1s tick and the
		// dialog would stop updating every second.
		byMAC, _ := neighSnapshot()
		arp := "down"
		if e, ok := byMAC[store.NormMAC(dev.MAC)]; ok {
			arp = e.State
		}
		var icmp *icmpResult
		probes := make([]probeView, len(dev.Probes))
		var wg sync.WaitGroup
		if dev.IP != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				icmp = probeICMP(dev.IP)
			}()
		}
		for i, p := range dev.Probes {
			wg.Add(1)
			go func(i, p int) {
				defer wg.Done()
				probes[i] = probeOne(dev.IP, p)
			}(i, p)
		}
		wg.Wait()

		// Awake = an ICMP reply or any TCP watchdog open. ARP is deliberately
		// NOT a wake signal: a NIC can ARP-reply via offload while the host is
		// still asleep, so we keep sending until the OS/services actually answer.
		up := liveUp(icmp, probes)
		done := up || elapsed >= int(wakeTimeout.Seconds())

		s.hub.Broadcast("wake", wakeEvent{
			MAC: dev.MAC, Elapsed: elapsed, Timeout: int(wakeTimeout.Seconds()), Packets: packets,
			Arp: arp, Icmp: icmp, Probes: probes, Up: up, Done: done,
		})

		if done {
			return
		}
		// Stop being noisy if nobody is watching any more.
		if !s.hub.Active() {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if elapsed > 0 && (elapsed+1)%wakeResendEv == 0 {
				send()
			}
		}
	}
}
