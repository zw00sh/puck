// Package netconf auto-derives the LAN interface's addressing so puckview never
// asks the user for a netmask (the hand-entered netmask was a recurring WoL
// failure). Broadcast/CIDR come straight from the kernel's view of the iface.
package netconf

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

// Info is the box's network identity as puckview understands it.
type Info struct {
	Iface     string   `json:"iface"`
	IP        string   `json:"ip"`        // primary IPv4 on the LAN iface
	CIDR      string   `json:"cidr"`      // e.g. 192.168.50.0/24
	Broadcast string   `json:"broadcast"` // subnet-directed broadcast, e.g. 192.168.50.255
	Gateway   string   `json:"gateway"`   // default route next-hop
	DNS       []string `json:"dns"`       // resolvers from the system config
	TSIP      string   `json:"ts_ip"`     // tailnet (100.64/10) address, if any

	ipNet *net.IPNet
}

// IPNet is the parsed CIDR of the LAN interface.
func (i Info) IPNet() *net.IPNet { return i.ipNet }

// Detect inspects the interfaces and picks the LAN interface. If prefIface is
// set it is used verbatim; otherwise the first up, non-loopback interface with a
// private IPv4 address wins (tailscale/docker interfaces are skipped).
func Detect(prefIface string) (Info, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return Info{}, err
	}

	var chosen *net.Interface
	var chosenIP net.IP
	var chosenNet *net.IPNet
	var tsIP string

	for i := range ifaces {
		ifc := ifaces[i]
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil {
				continue
			}
			if isTailnet(ip4) {
				tsIP = ip4.String()
				continue
			}
			if !ip4.IsPrivate() {
				continue
			}
			if skipIface(ifc.Name) {
				continue
			}
			if prefIface != "" && ifc.Name != prefIface {
				continue
			}
			if chosen == nil {
				c := ifc
				chosen, chosenIP, chosenNet = &c, ip4, ipn
			}
		}
	}

	if chosen == nil {
		return Info{}, errors.New("no suitable LAN interface found")
	}

	info := Info{
		Iface:     chosen.Name,
		IP:        chosenIP.String(),
		CIDR:      (&net.IPNet{IP: chosenIP.Mask(chosenNet.Mask), Mask: chosenNet.Mask}).String(),
		Broadcast: Broadcast(chosenIP, chosenNet.Mask).String(),
		TSIP:      tsIP,
		ipNet:     chosenNet,
	}
	info.Gateway = defaultGateway() // platform-specific
	info.DNS = systemDNS()
	return info, nil
}

// systemDNS parses /etc/resolv.conf for nameserver entries (present on darwin and
// linux alike).
func systemDNS() []string {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "nameserver") {
			if parts := strings.Fields(line); len(parts) == 2 {
				out = append(out, parts[1])
			}
		}
	}
	return out
}

// Broadcast computes the subnet-directed broadcast for ip within mask.
func Broadcast(ip net.IP, mask net.IPMask) net.IP {
	ip = ip.To4()
	b := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		b[i] = ip[i] | ^mask[i]
	}
	return b
}

// isTailnet reports whether ip is in the Tailscale CGNAT range 100.64.0.0/10.
func isTailnet(ip net.IP) bool {
	ip = ip.To4()
	return ip != nil && ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127
}

func skipIface(name string) bool {
	for _, p := range []string{"tailscale", "utun", "docker", "br-", "veth", "lo"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// SubnetSize returns the number of host addresses in an IPv4 CIDR.
func SubnetSize(cidr string) (int, error) {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return 0, err
	}
	ones, bits := n.Mask.Size()
	if bits != 32 {
		return 0, fmt.Errorf("not an IPv4 network: %s", cidr)
	}
	return 1 << (bits - ones), nil
}
