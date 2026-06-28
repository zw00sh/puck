package wol

import (
	"bytes"
	"net"
	"testing"
)

func TestMagicPayload(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	p := MagicPayload(mac)

	if len(p) != 102 { // 6 sync + 16×6
		t.Fatalf("length = %d, want 102", len(p))
	}
	for i := 0; i < 6; i++ {
		if p[i] != 0xFF {
			t.Fatalf("sync byte %d = %#x, want 0xFF", i, p[i])
		}
	}
	for i := 0; i < 16; i++ {
		off := 6 + i*6
		if !bytes.Equal(p[off:off+6], mac) {
			t.Fatalf("mac repetition %d = % x, want % x", i, p[off:off+6], mac)
		}
	}
}
