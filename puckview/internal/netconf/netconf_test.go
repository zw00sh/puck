package netconf

import (
	"net"
	"testing"
)

func TestBroadcast(t *testing.T) {
	cases := []struct {
		ip, mask, want string
	}{
		{"192.168.50.10", "255.255.255.0", "192.168.50.255"},
		{"10.0.0.5", "255.0.0.0", "10.255.255.255"},
		{"172.16.4.9", "255.255.0.0", "172.16.255.255"},
		{"192.168.1.130", "255.255.255.128", "192.168.1.255"},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		mask := net.IPMask(net.ParseIP(c.mask).To4())
		if got := Broadcast(ip, mask).String(); got != c.want {
			t.Errorf("Broadcast(%s/%s) = %s, want %s", c.ip, c.mask, got, c.want)
		}
	}
}

func TestSubnetSize(t *testing.T) {
	cases := []struct {
		cidr string
		want int
	}{
		{"192.168.50.0/24", 256},
		{"10.0.0.0/22", 1024},
		{"172.16.0.0/16", 65536},
		{"192.168.1.0/30", 4},
	}
	for _, c := range cases {
		got, err := SubnetSize(c.cidr)
		if err != nil {
			t.Errorf("SubnetSize(%s) error: %v", c.cidr, err)
			continue
		}
		if got != c.want {
			t.Errorf("SubnetSize(%s) = %d, want %d", c.cidr, got, c.want)
		}
	}
}

func TestIsTailnet(t *testing.T) {
	yes := []string{"100.64.0.1", "100.100.100.100", "100.127.255.255"}
	no := []string{"100.63.0.1", "100.128.0.1", "192.168.1.1", "10.0.0.1"}
	for _, s := range yes {
		if !isTailnet(net.ParseIP(s)) {
			t.Errorf("isTailnet(%s) = false, want true", s)
		}
	}
	for _, s := range no {
		if isTailnet(net.ParseIP(s)) {
			t.Errorf("isTailnet(%s) = true, want false", s)
		}
	}
}
