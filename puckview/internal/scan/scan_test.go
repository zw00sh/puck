package scan

import (
	"net"
	"testing"
)

func mustNet(s string) *net.IPNet { _, n, _ := net.ParseCIDR(s); return n }

func TestValidate(t *testing.T) {
	local := mustNet("192.168.50.0/24")

	ok := []string{"192.168.50.0/24", "192.168.50.0/26", "192.168.50.64/28"}
	for _, c := range ok {
		if err := Validate(c, local); err != nil {
			t.Errorf("Validate(%s) unexpected error: %v", c, err)
		}
	}

	bad := []string{
		"172.31.0.0/24",   // off-subnet
		"10.0.0.0/8",      // too large + off-subnet
		"192.168.50.0/20", // too large (4096 hosts) — still within? /20 contains .50.0 but >MaxHosts
		"not-a-cidr",
	}
	for _, c := range bad {
		if err := Validate(c, local); err == nil {
			t.Errorf("Validate(%s) = nil error, want rejection", c)
		}
	}
}

func TestEnumerateSkipsNetworkAndBroadcast(t *testing.T) {
	hosts := enumerate(mustNet("192.168.50.0/29")) // .0-.7 → usable .1-.6
	if len(hosts) != 6 {
		t.Fatalf("got %d hosts, want 6: %v", len(hosts), hosts)
	}
	for _, h := range hosts {
		if h == "192.168.50.0" || h == "192.168.50.7" {
			t.Errorf("enumerate included network/broadcast: %s", h)
		}
	}
}
