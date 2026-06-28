//go:build linux

package neigh

import (
	"bufio"
	"os"
	"strings"
)

// readProc parses /proc/net/arp as a fallback when netlink is unavailable.
// It yields no last-seen information (SeenSec stays -1).
func readProc() ([]Entry, error) {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		ip, flags, mac := fields[0], fields[2], fields[3]
		state := "down"
		if flags == "0x2" { // ATF_COM — entry complete
			state = "stale"
		}
		if mac == "00:00:00:00:00:00" {
			continue
		}
		out = append(out, Entry{IP: ip, MAC: normMAC(mac), State: state, SeenSec: -1})
	}
	return out, sc.Err()
}
