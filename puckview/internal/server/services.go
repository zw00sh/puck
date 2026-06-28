package server

import (
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/zw00sh/puck/puckview/internal/catalogue"
	"github.com/zw00sh/puck/puckview/internal/probe"
	"github.com/zw00sh/puck/puckview/internal/tailscale"
)

// serviceView is one SERVICES row: a tailnet link plus a health dot.
type serviceView struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Detail string `json:"detail"`
	Up     bool   `json:"up"`
}

// refreshMeta refreshes the cached tailnet state and service catalogue and
// broadcasts the SERVICES frame. tailscale/catalogue reads are passive; the
// per-service health check is a local TCP connect (loopback), which is cheap and
// not "spurious" LAN traffic.
func (s *Server) refreshMeta() {
	ts := tailscale.Status()

	// SERVICES is the provisioned catalogue only. We deliberately do NOT merge in
	// `tailscale serve` handlers: they list puckview's own front door and duplicate
	// catalogue entries by backend, which read as extraneous services.
	entries := catalogue.Load(s.cfg.Catalogue)
	var svcs []serviceView
	for _, e := range entries {
		health := e.Health
		if health == "" {
			health = hostPortFromURL(e.URL)
		}
		svcs = append(svcs, serviceView{
			Name:   e.Name,
			URL:    e.URL,
			Detail: e.Detail,
			Up:     healthUp(health),
		})
	}

	s.metaMu.Lock()
	s.tsInfo = ts
	s.services = svcs
	s.metaMu.Unlock()

	s.hub.Broadcast("services", svcs)
}

func (s *Server) getTS() *tailscale.Info {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	return s.tsInfo
}

func (s *Server) snapshotServices() []serviceView {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	return s.services
}

func healthUp(hostPort string) bool {
	if hostPort == "" {
		return false
	}
	host, port := splitHostPort(hostPort)
	if port == 0 {
		return false
	}
	up, _ := probe.TCP(host, port, 600*time.Millisecond)
	return up
}

func hostPortFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Port() == "" {
		switch u.Scheme {
		case "https":
			return u.Hostname() + ":443"
		case "http":
			return u.Hostname() + ":80"
		}
	}
	return u.Host
}

func splitHostPort(hp string) (string, int) {
	host, portStr, err := net.SplitHostPort(hp)
	if err != nil {
		return hp, 0
	}
	port, _ := strconv.Atoi(portStr)
	if host == "" {
		host = "127.0.0.1"
	}
	return host, port
}
