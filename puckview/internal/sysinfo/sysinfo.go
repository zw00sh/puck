// Package sysinfo samples box diagnostics from the kernel. All reads are passive
// (/proc, /sys, statfs) and emit no network traffic, so they run whenever a
// dashboard client is connected. The Sampler keeps short rolling histories for
// the CPU/network sparklines the UI draws.
package sysinfo

import (
	"sync"
	"syscall"
	"time"
)

const histLen = 24 // sparkline width

type CPU struct {
	Pct   float64    `json:"pct"`
	Cores []float64  `json:"cores"`
	Load  [3]float64 `json:"load"`
	TempC float64    `json:"temp"`
}

type Mem struct {
	Pct      float64 `json:"pct"`
	UsedMB   int     `json:"usedMB"`
	TotalMB  int     `json:"totalMB"`
	Disabled bool    `json:"disabled"` // e.g. no swap configured
}

type Disk struct {
	Pct     float64 `json:"pct"`
	UsedGB  float64 `json:"usedGB"`
	TotalGB float64 `json:"totalGB"`
}

type Net struct {
	RxKBs float64   `json:"rxKBs"` // current rx in KB/s
	TxKBs float64   `json:"txKBs"`
	Rx    []float64 `json:"rx"` // history (KB/s)
	Tx    []float64 `json:"tx"`
}

type Stats struct {
	UptimeSec int64 `json:"uptimeSec"`
	CPU       CPU   `json:"cpu"`
	Mem       Mem   `json:"mem"`
	Swap      Mem   `json:"swap"`
	Disk      Disk  `json:"disk"`
	Net       Net   `json:"net"`
}

// cpuSnap is one reading of total/busy jiffies, overall plus per-core.
type cpuSnap struct {
	totalBusy, total uint64
	cores            [][2]uint64 // [busy, total] per core
}

type Sampler struct {
	iface string

	mu       sync.Mutex
	prevCPU  cpuSnap
	prevTime time.Time
	prevRx   uint64
	prevTx   uint64
	rxHist   []float64
	txHist   []float64
	primed   bool
}

func New(iface string) *Sampler {
	return &Sampler{
		iface:  iface,
		rxHist: make([]float64, histLen),
		txHist: make([]float64, histLen),
	}
}

// Sample takes a fresh reading, computing deltas against the previous call.
func (s *Sampler) Sample() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	var st Stats
	st.UptimeSec = uptimeSec()

	// CPU (delta of busy/total jiffies)
	cur := cpuSample()
	if s.primed {
		st.CPU.Pct = cpuPct(s.prevCPU.totalBusy, s.prevCPU.total, cur.totalBusy, cur.total)
		for i := range cur.cores {
			if i < len(s.prevCPU.cores) {
				st.CPU.Cores = append(st.CPU.Cores,
					cpuPct(s.prevCPU.cores[i][0], s.prevCPU.cores[i][1], cur.cores[i][0], cur.cores[i][1]))
			}
		}
	} else {
		st.CPU.Cores = make([]float64, len(cur.cores))
	}
	s.prevCPU = cur
	st.CPU.Load = loadAvg()
	st.CPU.TempC = tempC()

	st.Mem, st.Swap = memInfo()
	st.Disk = diskInfo("/")

	// Network throughput (bytes delta / elapsed)
	rx, tx := netCounters(s.iface)
	if s.primed && !s.prevTime.IsZero() {
		dt := now.Sub(s.prevTime).Seconds()
		if dt > 0 {
			st.Net.RxKBs = float64(rx-s.prevRx) / dt / 1024
			st.Net.TxKBs = float64(tx-s.prevTx) / dt / 1024
		}
	}
	s.prevRx, s.prevTx, s.prevTime = rx, tx, now
	s.rxHist = append(s.rxHist[1:], st.Net.RxKBs)
	s.txHist = append(s.txHist[1:], st.Net.TxKBs)
	st.Net.Rx = append([]float64(nil), s.rxHist...)
	st.Net.Tx = append([]float64(nil), s.txHist...)

	s.primed = true
	return st
}

func cpuPct(prevBusy, prevTotal, busy, total uint64) float64 {
	dt := float64(total - prevTotal)
	if dt <= 0 {
		return 0
	}
	p := float64(busy-prevBusy) / dt * 100
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p
}

// diskInfo reports usage for the filesystem containing path via statfs (passive,
// available on darwin and linux alike).
func diskInfo(path string) Disk {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return Disk{}
	}
	total := float64(s.Blocks) * float64(s.Bsize)
	free := float64(s.Bavail) * float64(s.Bsize)
	used := total - free
	d := Disk{TotalGB: total / 1e9, UsedGB: used / 1e9}
	if total > 0 {
		d.Pct = used / total * 100
	}
	return d
}
