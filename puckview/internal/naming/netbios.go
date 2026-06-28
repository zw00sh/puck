package naming

import (
	"net"
	"strings"
	"time"
)

// netbiosName sends a NBSTAT (node status) query to UDP 137 and returns the
// host's registered unique workstation name. Windows hosts answer this even when
// they drop ICMP, which makes it a useful complement to rDNS. Best-effort: any
// error or malformed reply yields "".
func netbiosName(ip string, timeout time.Duration) string {
	conn, err := net.DialTimeout("udp", net.JoinHostPort(ip, "137"), timeout)
	if err != nil {
		return ""
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	if _, err := conn.Write(nbstatQuery()); err != nil {
		return ""
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return ""
	}
	return parseNBStat(buf[:n])
}

// nbstatQuery builds a node-status request for the wildcard name "*".
func nbstatQuery() []byte {
	q := []byte{
		0x00, 0x00, // transaction id
		0x00, 0x00, // flags
		0x00, 0x01, // questions
		0x00, 0x00, // answer RRs
		0x00, 0x00, // authority RRs
		0x00, 0x00, // additional RRs
		0x20, // name length (32)
	}
	// First-level encoded "*" + 15 NUL bytes: each nibble + 'A'.
	name := make([]byte, 16)
	name[0] = '*'
	for _, b := range name {
		q = append(q, 'A'+(b>>4), 'A'+(b&0x0f))
	}
	q = append(q,
		0x00,       // name terminator
		0x00, 0x21, // type NBSTAT
		0x00, 0x01, // class IN
	)
	return q
}

// parseNBStat extracts the unique (non-group) workstation name from a node
// status response.
func parseNBStat(b []byte) string {
	// header(12) + echoed question. The question is: encoded name (34) + type(2)
	// + class(2). Then the RR: name ptr/encoded, type(2), class(2), ttl(4),
	// rdlength(2), rdata. rdata starts with a 1-byte name count.
	const qLen = 34 + 4
	off := 12 + qLen
	if len(b) < off+12 {
		return ""
	}
	// Skip RR name (same 34-byte encoding) + type/class/ttl/rdlength = 34+10.
	off += 34 + 2 + 2 + 4 + 2
	if off >= len(b) {
		return ""
	}
	count := int(b[off])
	off++
	for i := 0; i < count; i++ {
		if off+18 > len(b) {
			break
		}
		name := strings.TrimRight(string(b[off:off+15]), " \x00")
		suffix := b[off+15]
		flags := uint16(b[off+16])<<8 | uint16(b[off+17])
		off += 18
		isGroup := flags&0x8000 != 0
		// suffix 0x00 = workstation service; unique (non-group) is the host name.
		if suffix == 0x00 && !isGroup && name != "" {
			return name
		}
	}
	return ""
}
