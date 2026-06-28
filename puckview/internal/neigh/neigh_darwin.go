//go:build !linux

// Development implementation for macOS: parse `arp -an`. macOS exposes no
// last-seen timestamp, so SeenSec stays -1 and freshness reads as "stale".
package neigh

import (
	"os/exec"
	"regexp"
)

var arpLine = regexp.MustCompile(`\(([0-9.]+)\) at ([0-9a-fA-F:]+)`)

func read() ([]Entry, error) {
	out, err := exec.Command("arp", "-an").Output()
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, m := range arpLine.FindAllStringSubmatch(string(out), -1) {
		mac := normMAC(m[2])
		if mac == "" || mac == "ff:ff:ff:ff:ff:ff" {
			continue
		}
		entries = append(entries, Entry{IP: m[1], MAC: mac, State: "stale", SeenSec: -1})
	}
	return entries, nil
}
