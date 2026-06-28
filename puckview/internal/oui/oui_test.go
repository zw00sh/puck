package oui

import (
	"strings"
	"testing"
)

// Vendor names come straight from the IEEE registry, so assert a distinctive
// substring (case-insensitive) rather than the exact, verbose org name — that
// stays robust across table refreshes.
func TestLookupVendor(t *testing.T) {
	cases := map[string]string{
		"18:c0:4d:00:00:01": "giga-byte",    // 18C04D = Gigabyte OUI
		"b8:27:eb:00:00:01": "raspberry pi", // B827EB = Raspberry Pi OUI
	}
	for mac, want := range cases {
		got := Lookup(mac)
		if !strings.Contains(strings.ToLower(got), want) {
			t.Errorf("Lookup(%s) = %q, want a name containing %q", mac, got, want)
		}
	}
}

func TestLookupSpecial(t *testing.T) {
	cases := map[string]string{
		// Locally-administered (U/L bit set) → randomized, no vendor.
		"8e:00:00:00:00:01": "(randomized)",
		"2e:00:00:00:00:01": "(randomized)",
		// Unknown prefix (globally-administered) → empty.
		"00:de:ad:be:ef:01": "",
	}
	for mac, want := range cases {
		if got := Lookup(mac); got != want {
			t.Errorf("Lookup(%s) = %q, want %q", mac, got, want)
		}
	}
}
