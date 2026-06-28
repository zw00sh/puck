package server

import (
	"sync"
	"time"

	"github.com/zw00sh/puck/puckview/internal/naming"
)

// The name cache holds resolved host names for discovered IPs so the ARP CACHE
// and SCAN views can show names without re-querying DNS on every frame. It is
// populated only during active sessions (rDNS/NetBIOS are active queries).

const nameTTL = 10 * time.Minute

type nameEntry struct {
	name string
	at   time.Time
}

func (s *Server) cachedName(ip string) string {
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	if e, ok := s.nameCache[ip]; ok && time.Since(e.at) < nameTTL {
		return e.name
	}
	return ""
}

func (s *Server) setName(ip, name string) {
	s.nameMu.Lock()
	s.nameCache[ip] = nameEntry{name: name, at: time.Now()}
	s.nameMu.Unlock()
}

func (s *Server) needsName(ip string) bool {
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	e, ok := s.nameCache[ip]
	return !ok || time.Since(e.at) >= nameTTL
}

// resolveNames resolves (in parallel, capped) any of the given IPs that don't
// have a fresh cached name. Active traffic — callers gate it on a live session.
// Negative results are cached too (as "") to avoid hammering the resolver for
// hosts with no PTR/NetBIOS name.
func (s *Server) resolveNames(ips []string) {
	const maxConcurrent = 8
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for _, ip := range ips {
		if ip == "" || !s.needsName(ip) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			name, _ := naming.Name(ip, s.lanResolver(), resolveTimeout)
			s.setName(ip, name)
		}(ip)
	}
	wg.Wait()
}
