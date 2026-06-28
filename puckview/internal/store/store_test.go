package store

import (
	"path/filepath"
	"testing"
)

func TestNormMAC(t *testing.T) {
	cases := map[string]string{
		"AA:BB:CC:DD:EE:FF":    "aa:bb:cc:dd:ee:ff",
		"AA-BB-CC-DD-E-FF":     "aa:bb:cc:dd:0e:ff", // unpadded octet + dashes (macOS arp form)
		"  11:22:33:44:55:66 ": "11:22:33:44:55:66",
		"a:b:c:d:e:f":          "0a:0b:0c:0d:0e:0f",
	}
	for in, want := range cases {
		if got := NormMAC(in); got != want {
			t.Errorf("NormMAC(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeviceLifecycle(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.Add(Device{MAC: "AA:BB:CC:DD:EE:FF", IP: "192.0.2.59", Name: "test-pc"}); err != nil {
		t.Fatal(err)
	}
	d, ok, _ := st.Get("aa:bb:cc:dd:ee:ff")
	if !ok || d.Name != "test-pc" || d.IP != "192.0.2.59" {
		t.Fatalf("Get returned %+v ok=%v", d, ok)
	}

	// Probes
	st.AddProbe("aa:bb:cc:dd:ee:ff", 3389)
	st.AddProbe("aa:bb:cc:dd:ee:ff", 3389) // idempotent
	d, _, _ = st.Get("aa:bb:cc:dd:ee:ff")
	if len(d.Probes) != 1 || d.Probes[0] != 3389 {
		t.Fatalf("probes = %v, want [3389]", d.Probes)
	}
	st.DeleteProbe("aa:bb:cc:dd:ee:ff", 3389)
	d, _, _ = st.Get("aa:bb:cc:dd:ee:ff")
	if len(d.Probes) != 0 {
		t.Fatalf("probes after delete = %v, want []", d.Probes)
	}

	// IP reconciliation
	if err := st.ReconcileIP("aa:bb:cc:dd:ee:ff", "192.0.2.77"); err != nil {
		t.Fatal(err)
	}
	d, _, _ = st.Get("aa:bb:cc:dd:ee:ff")
	if d.IP != "192.0.2.77" {
		t.Fatalf("IP after reconcile = %s, want 192.0.2.77", d.IP)
	}

	// Delete cascades probes
	st.AddProbe("aa:bb:cc:dd:ee:ff", 22)
	st.Delete("aa:bb:cc:dd:ee:ff")
	if _, ok, _ := st.Get("aa:bb:cc:dd:ee:ff"); ok {
		t.Fatal("device still present after delete")
	}
}
