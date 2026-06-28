//go:build linux

package sysinfo

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

func uptimeSec() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return int64(v)
}

func cpuSample() cpuSnap {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSnap{}
	}
	defer f.Close()
	var snap cpuSnap
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu") {
			break
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		var total, busy uint64
		var idle uint64
		for i, fld := range fields[1:] {
			v, _ := strconv.ParseUint(fld, 10, 64)
			total += v
			if i == 3 || i == 4 { // idle + iowait
				idle += v
			}
		}
		busy = total - idle
		if fields[0] == "cpu" {
			snap.totalBusy, snap.total = busy, total
		} else {
			snap.cores = append(snap.cores, [2]uint64{busy, total})
		}
	}
	return snap
}

func loadAvg() [3]float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return [3]float64{}
	}
	f := strings.Fields(string(b))
	var l [3]float64
	for i := 0; i < 3 && i < len(f); i++ {
		l[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return l
}

// tempC reads the first thermal zone (SoC temp). Returns 0 if unavailable.
func tempC() float64 {
	b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return 0
	}
	return v / 1000
}

func memInfo() (mem, swap Mem) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()
	vals := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) < 2 {
			continue
		}
		key := strings.TrimSuffix(parts[0], ":")
		v, _ := strconv.ParseUint(parts[1], 10, 64) // kB
		vals[key] = v
	}
	memTotal := vals["MemTotal"]
	memAvail := vals["MemAvailable"]
	if memTotal > 0 {
		used := memTotal - memAvail
		mem = Mem{Pct: float64(used) / float64(memTotal) * 100, UsedMB: int(used / 1024), TotalMB: int(memTotal / 1024)}
	}
	swTotal := vals["SwapTotal"]
	swFree := vals["SwapFree"]
	if swTotal > 0 {
		used := swTotal - swFree
		swap = Mem{Pct: float64(used) / float64(swTotal) * 100, UsedMB: int(used / 1024), TotalMB: int(swTotal / 1024)}
	} else {
		swap = Mem{Disabled: true}
	}
	return
}

func netCounters(iface string) (rx, tx uint64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		if name != iface {
			continue
		}
		fields := strings.Fields(line[idx+1:])
		if len(fields) >= 9 {
			rx, _ = strconv.ParseUint(fields[0], 10, 64)
			tx, _ = strconv.ParseUint(fields[8], 10, 64)
		}
		return
	}
	return
}
