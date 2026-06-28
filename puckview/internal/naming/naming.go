// Package naming resolves human names for a host, best-effort and layered:
// reverse DNS (queried against the LAN resolver, never the box's MagicDNS
// resolver — bare names resolve to tailnet IPs there), then NetBIOS, with the
// OUI vendor as a always-available fallback label.
package naming

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/zw00sh/puck/puckview/internal/oui"
)

type Result struct {
	Name   string `json:"name"`
	Source string `json:"source"` // rdns | netbios | ""
	Vendor string `json:"vendor"`
}

// Resolve runs the layered resolvers for ip/mac. lanResolver is the IP of the
// LAN DNS server (gateway / Pi-hole); if empty, the system resolver is used.
func Resolve(ip, mac, lanResolver string, timeout time.Duration) Result {
	r := Result{Vendor: oui.Lookup(mac)}
	if ip == "" {
		return r
	}
	r.Name, r.Source = Name(ip, lanResolver, timeout)
	return r
}

// Name resolves just the host name for an IP (rDNS, then NetBIOS), returning the
// name and its source. Used to enrich discovery results where the vendor is
// already known from the MAC.
func Name(ip, lanResolver string, timeout time.Duration) (string, string) {
	if name := reverseDNS(ip, lanResolver, timeout); name != "" {
		return name, "rdns"
	}
	if name := netbiosName(ip, timeout); name != "" {
		return name, "netbios"
	}
	return "", ""
}

func reverseDNS(ip, lanResolver string, timeout time.Duration) string {
	resolver := net.DefaultResolver
	if lanResolver != "" {
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: timeout}
				return d.DialContext(ctx, "udp", net.JoinHostPort(lanResolver, "53"))
			},
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	names, err := resolver.LookupAddr(ctx, ip)
	if err != nil || len(names) == 0 {
		return ""
	}
	return shortName(names[0])
}

// shortName trims the trailing dot and the domain, leaving the leftmost label.
func shortName(fqdn string) string {
	fqdn = strings.TrimSuffix(fqdn, ".")
	if i := strings.IndexByte(fqdn, '.'); i > 0 {
		return fqdn[:i]
	}
	return fqdn
}
