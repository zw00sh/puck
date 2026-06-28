//go:build linux

package netconf

import (
	"bufio"
	"encoding/hex"
	"net"
	"os"
	"strings"
)

// defaultGateway reads /proc/net/route for the 0.0.0.0 destination's gateway.
// The gateway field is the 32-bit address stored little-endian as hex.
func defaultGateway() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || fields[1] != "00000000" { // destination 0.0.0.0
			continue
		}
		raw, err := hex.DecodeString(fields[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		return net.IPv4(raw[3], raw[2], raw[1], raw[0]).String()
	}
	return ""
}
