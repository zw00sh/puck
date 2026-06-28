// Package scan performs an active, kernel-assisted ARP sweep of a local subnet.
// Rather than crafting ARP frames itself, it UDP-nudges each host so the kernel
// performs the ARP (delegating retransmit/timeout/state to the kernel), then
// re-reads the neighbour cache. This keeps the scanner small and reliable.
package scan

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/zw00sh/puck/puckview/internal/neigh"
	"github.com/zw00sh/puck/puckview/internal/netconf"
)

// MaxHosts caps a sweep so an oversized subnet can't flood the LAN.
const MaxHosts = 1024 // /22

// Progress is called as the sweep advances.
type Progress func(done, total int, current string)

// Validate checks that target is a usable IPv4 CIDR contained within one of the
// box's own subnets and is not larger than MaxHosts.
func Validate(target string, local *net.IPNet) error {
	ip, ipnet, err := net.ParseCIDR(target)
	if err != nil {
		return fmt.Errorf("invalid CIDR")
	}
	if ip.To4() == nil {
		return fmt.Errorf("IPv4 only")
	}
	if local != nil && !local.Contains(ipnet.IP) {
		return fmt.Errorf("target %s is not on the local subnet %s", target, local.String())
	}
	if size, _ := netconf.SubnetSize(ipnet.String()); size > MaxHosts {
		return fmt.Errorf("subnet too large (%d > %d hosts)", size, MaxHosts)
	}
	return nil
}

// Sweep nudges every host in target and returns the neighbour entries that fall
// within it once the kernel has resolved them.
func Sweep(ctx context.Context, target string, prog Progress) ([]neigh.Entry, error) {
	_, ipnet, err := net.ParseCIDR(target)
	if err != nil {
		return nil, err
	}
	hosts := enumerate(ipnet)
	total := len(hosts)

	const workers = 64
	var wg sync.WaitGroup
	jobs := make(chan string)
	var done int
	var mu sync.Mutex

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range jobs {
				nudge(ip)
				mu.Lock()
				done++
				d := done
				mu.Unlock()
				if prog != nil {
					prog(d, total, ip)
				}
			}
		}()
	}

	for _, h := range hosts {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, ctx.Err()
		case jobs <- h:
		}
	}
	close(jobs)
	wg.Wait()

	// Let the kernel finish resolving before reading the cache back.
	time.Sleep(600 * time.Millisecond)

	all, err := neigh.Read()
	if err != nil {
		return nil, err
	}
	var out []neigh.Entry
	for _, e := range all {
		if ip := net.ParseIP(e.IP); ip != nil && ipnet.Contains(ip) {
			out = append(out, e)
		}
	}
	return out, nil
}

// nudge sends a single UDP datagram to provoke the kernel into ARP-resolving ip.
func nudge(ip string) {
	c, err := net.DialTimeout("udp", net.JoinHostPort(ip, "9"), 200*time.Millisecond)
	if err != nil {
		return
	}
	c.Write([]byte{0})
	c.Close()
}

func enumerate(ipnet *net.IPNet) []string {
	var out []string
	ip := ipnet.IP.Mask(ipnet.Mask).To4()
	if ip == nil {
		return out
	}
	network := dup(ip)
	bcast := netconf.Broadcast(ip, ipnet.Mask)
	for cur := dup(network); ipnet.Contains(cur); inc(cur) {
		if cur.Equal(network) || cur.Equal(bcast) {
			continue
		}
		out = append(out, cur.String())
	}
	return out
}

func dup(ip net.IP) net.IP { c := make(net.IP, len(ip)); copy(c, ip); return c }
func inc(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
}
