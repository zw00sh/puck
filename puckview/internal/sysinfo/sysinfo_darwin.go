//go:build !linux

// Development stub for non-Linux hosts (the Mac dev box). Disk is real via
// statfs; CPU/mem/temp/net are synthesized so the dashboard looks alive while
// iterating on the UI. The production target is always linux/arm64.
package sysinfo

import (
	"math"
	"math/rand"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var startTime = time.Now()

func uptimeSec() int64 { return int64(time.Since(startTime).Seconds()) + 86400 }

func cpuSample() cpuSnap {
	// Fabricate monotonically increasing jiffies so deltas yield a wandering %.
	t := uint64(time.Since(startTime).Milliseconds())
	jitter := func(seed int) uint64 {
		return uint64(float64(t) * (0.2 + 0.3*math.Abs(math.Sin(float64(seed)+float64(t)/4000))))
	}
	snap := cpuSnap{totalBusy: jitter(0), total: t}
	for i := 0; i < 4; i++ {
		snap.cores = append(snap.cores, [2]uint64{jitter(i + 1), t})
	}
	return snap
}

func loadAvg() [3]float64 {
	return [3]float64{0.2 + rand.Float64()*0.3, 0.25, 0.2}
}

func tempC() float64 { return 45 + rand.Float64()*8 }

func memInfo() (mem, swap Mem) {
	pct := 40 + rand.Float64()*10
	mem = Mem{Pct: pct, UsedMB: int(pct / 100 * 2048), TotalMB: 2048}
	swap = Mem{Pct: 2, UsedMB: 10, TotalMB: 512}
	return
}

// netCounters reads real per-interface byte counters via `netstat` so the dev
// build shows genuine throughput (on macOS the LAN iface is typically en0/Wi-Fi).
func netCounters(iface string) (rx, tx uint64) {
	out, err := exec.Command("netstat", "-ibnI", iface).Output()
	if err != nil {
		return 0, 0
	}
	for _, ln := range strings.Split(string(out), "\n")[1:] {
		f := strings.Fields(ln)
		// Columns: Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes ...
		// Use the hardware (<Link#n>) line to avoid double-counting per-AF rows.
		if len(f) < 10 || f[0] != iface || !strings.HasPrefix(f[2], "<Link") {
			continue
		}
		rx, _ = strconv.ParseUint(f[6], 10, 64)
		tx, _ = strconv.ParseUint(f[9], 10, 64)
		return rx, tx
	}
	return 0, 0
}
