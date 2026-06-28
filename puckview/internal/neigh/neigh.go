// Package neigh reads the kernel's neighbour (ARP) cache. This is the primary,
// firewall-proof presence signal and it emits no packets — a pure read of kernel
// state — so it is safe to run even when no dashboard client is connected.
package neigh

import (
	"fmt"
	"strings"
)

// Entry is one neighbour-cache record. State is normalised to up/stale/down.
// SeenSec is seconds since the kernel last confirmed reachability (-1 unknown).
type Entry struct {
	IP      string `json:"ip"`
	MAC     string `json:"mac"`
	State   string `json:"state"`
	SeenSec int64  `json:"seen_sec"`
}

// Read returns the current neighbour cache (platform-specific implementation).
func Read() ([]Entry, error) { return read() }

// normMAC canonicalises to lowercase, colon-separated, zero-padded octets so a
// neigh-cache MAC matches the store's identity regardless of platform format.
func normMAC(m string) string {
	m = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(m, "-", ":")))
	parts := strings.Split(m, ":")
	if len(parts) != 6 {
		return m
	}
	for i, p := range parts {
		if len(p) == 1 {
			parts[i] = "0" + p
		}
	}
	return strings.Join(parts, ":")
}

// FmtSeen renders a seconds-ago count as a compact human string.
func FmtSeen(sec int64) string {
	switch {
	case sec < 0:
		return "—"
	case sec < 5:
		return "now"
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh", sec/3600)
	default:
		return fmt.Sprintf("%dd", sec/86400)
	}
}
