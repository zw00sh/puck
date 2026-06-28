// Package tailscale reads tailnet state by shelling out to the installed
// `tailscale` CLI (status). Shelling out keeps puckview free of the large
// tailscale client dependency and stays purely read-only/passive.
package tailscale

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"
)

type Info struct {
	State    string `json:"state"`     // BackendState, e.g. "Running"
	Name     string `json:"name"`      // MagicDNS short name
	MagicDNS bool   `json:"magicdns"`  // whether a MagicDNS name is assigned
	ExitNode bool   `json:"exit_node"` // this node offers exit-node service
}

// Status returns the current tailnet state, or nil if the CLI is unavailable.
func Status() *Info {
	out, err := run("status", "--json")
	if err != nil {
		return nil
	}
	var raw struct {
		BackendState string
		Self         struct {
			DNSName        string
			ExitNodeOption bool
		}
	}
	if json.Unmarshal(out, &raw) != nil {
		return nil
	}
	info := &Info{
		State:    raw.BackendState,
		ExitNode: raw.Self.ExitNodeOption,
	}
	dns := strings.TrimSuffix(raw.Self.DNSName, ".")
	if dns != "" {
		info.MagicDNS = true
		if i := strings.IndexByte(dns, '.'); i > 0 {
			info.Name = dns[:i]
		} else {
			info.Name = dns
		}
	}
	return info
}

func run(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "tailscale", args...).Output()
}
