// Package catalogue is the provisioned service list — the one thing that IS
// baked in (by the Ansible role), as opposed to the dynamic device store. It is
// merged with live `tailscale serve` handlers and each entry gets a TCP health
// check.
package catalogue

import (
	"encoding/json"
	"os"
)

// Entry is one catalogued service. Health is a host:port the dashboard TCP-probes
// for the status dot; URL is the tailnet link shown to the user.
type Entry struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Detail string `json:"detail"`
	Health string `json:"health"`
}

// Load reads the catalogue JSON file. A missing/empty/invalid file yields nil
// (the catalogue is optional; serve-derived entries still appear).
func Load(path string) []Entry {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []Entry
	if json.Unmarshal(b, &entries) != nil {
		return nil
	}
	return entries
}
