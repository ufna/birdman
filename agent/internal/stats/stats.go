// Package stats samples node statistics for the agent heartbeat
// (docs/specs/protocol.md §1 NodeStats): cpu/mem/load from /proc, disk from
// statfs on the data dir. Best effort — on platforms or errors where a value
// cannot be read it stays zero (the master only records heartbeat liveness
// in v0).
package stats

import (
	"bytes"
	"os"
	"strconv"
	"sync"
)

// Stats is one node sample.
type Stats struct {
	CPU       float32 // busy fraction 0..1 since the previous sample
	MemUsed   uint64  // bytes (MemTotal - MemAvailable)
	MemTotal  uint64  // bytes
	DiskUsed  uint64  // bytes on the data dir filesystem
	DiskTotal uint64  // bytes
	Load1     float32
}

// Collector keeps the previous CPU counters to derive a busy fraction.
type Collector struct {
	diskPath string

	mu        sync.Mutex
	prevBusy  uint64
	prevTotal uint64
}

// New creates a collector; diskPath is the filesystem to report disk usage
// for (the agent data dir).
func New(diskPath string) *Collector {
	return &Collector{diskPath: diskPath}
}

// Sample reads the current node stats.
func (c *Collector) Sample() Stats {
	var s Stats
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		busy, total, ok := parseCPU(b)
		if ok {
			c.mu.Lock()
			if c.prevTotal > 0 && total > c.prevTotal {
				s.CPU = float32(busy-c.prevBusy) / float32(total-c.prevTotal)
			}
			c.prevBusy, c.prevTotal = busy, total
			c.mu.Unlock()
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		s.MemTotal, s.MemUsed = parseMem(b)
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		s.Load1 = parseLoad1(b)
	}
	s.DiskUsed, s.DiskTotal = diskUsage(c.diskPath)
	return s
}

// parseCPU extracts busy/total jiffies from the aggregate "cpu " line of
// /proc/stat: user nice system idle iowait irq softirq steal [guest ...].
func parseCPU(b []byte) (busy, total uint64, ok bool) {
	for _, line := range bytes.Split(b, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("cpu ")) {
			continue
		}
		fields := bytes.Fields(line[4:])
		if len(fields) < 5 {
			return 0, 0, false
		}
		var vals []uint64
		for _, f := range fields {
			v, err := strconv.ParseUint(string(f), 10, 64)
			if err != nil {
				return 0, 0, false
			}
			vals = append(vals, v)
		}
		var idle uint64 = vals[3] // idle
		if len(vals) > 4 {
			idle += vals[4] // + iowait
		}
		for i, v := range vals {
			if i >= 8 { // guest time is already included in user/nice
				break
			}
			total += v
		}
		return total - idle, total, true
	}
	return 0, 0, false
}

// parseMem extracts MemTotal and MemTotal-MemAvailable (bytes) from
// /proc/meminfo (values are reported in kB).
func parseMem(b []byte) (total, used uint64) {
	var avail uint64
	for _, line := range bytes.Split(b, []byte("\n")) {
		switch {
		case bytes.HasPrefix(line, []byte("MemTotal:")):
			total = parseMeminfoKB(line) << 10
		case bytes.HasPrefix(line, []byte("MemAvailable:")):
			avail = parseMeminfoKB(line) << 10
		}
	}
	if total >= avail {
		used = total - avail
	}
	return total, used
}

func parseMeminfoKB(line []byte) uint64 {
	fields := bytes.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(string(fields[1]), 10, 64)
	return v
}

// parseLoad1 extracts the 1-minute load average from /proc/loadavg.
func parseLoad1(b []byte) float32 {
	fields := bytes.Fields(b)
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(string(fields[0]), 32)
	return float32(v)
}
