// Package cgroups reads per-container cpu/mem usage from cgroups v2
// (docs/specs/agent.md §9: per-container metrics for :9101). The cgroup of a
// container is resolved through /proc/<pid>/cgroup of its init process — this
// works for any runc cgroup layout containerd happens to use.
package cgroups

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Usage is one cpu/mem sample of a cgroup.
type Usage struct {
	CPUSeconds float64 // cumulative cpu time (user+system), counter
	MemBytes   uint64  // current memory usage
}

// Read returns the usage of the cgroup the process pid belongs to.
// Linux + cgroups v2 only; on other systems it returns an error.
func Read(pid uint32) (Usage, error) {
	return read("/proc", "/sys/fs/cgroup", pid)
}

// read is the testable core: proc and cgroup filesystem roots are injected.
func read(procRoot, cgroupRoot string, pid uint32) (Usage, error) {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.FormatUint(uint64(pid), 10), "cgroup"))
	if err != nil {
		return Usage{}, err
	}
	rel, err := ParsePath(raw)
	if err != nil {
		return Usage{}, err
	}
	dir := filepath.Join(cgroupRoot, rel)

	var u Usage
	cpu, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return Usage{}, err
	}
	usec, err := ParseCPUStatUsec(cpu)
	if err != nil {
		return Usage{}, err
	}
	u.CPUSeconds = float64(usec) / 1e6

	mem, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return Usage{}, err
	}
	u.MemBytes, err = strconv.ParseUint(strings.TrimSpace(string(mem)), 10, 64)
	if err != nil {
		return Usage{}, fmt.Errorf("memory.current: %w", err)
	}
	return u, nil
}

// ParsePath extracts the unified-hierarchy (v2) cgroup path from a
// /proc/<pid>/cgroup file: the "0::<path>" line.
func ParsePath(procCgroup []byte) (string, error) {
	for _, line := range bytes.Split(procCgroup, []byte("\n")) {
		rest, ok := bytes.CutPrefix(line, []byte("0::"))
		if !ok {
			continue
		}
		p := strings.TrimSpace(string(rest))
		if p == "" || !strings.HasPrefix(p, "/") {
			return "", fmt.Errorf("malformed cgroup v2 line %q", line)
		}
		return p, nil
	}
	return "", fmt.Errorf("no cgroup v2 (0::) entry")
}

// ParseCPUStatUsec extracts usage_usec from a cgroup v2 cpu.stat file.
func ParseCPUStatUsec(cpuStat []byte) (uint64, error) {
	for _, line := range bytes.Split(cpuStat, []byte("\n")) {
		fields := bytes.Fields(line)
		if len(fields) == 2 && string(fields[0]) == "usage_usec" {
			return strconv.ParseUint(string(fields[1]), 10, 64)
		}
	}
	return 0, fmt.Errorf("cpu.stat: no usage_usec")
}
