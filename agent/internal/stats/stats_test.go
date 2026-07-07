package stats

import (
	"math"
	"testing"
)

func TestParseCPU(t *testing.T) {
	// user nice system idle iowait irq softirq steal
	b := []byte("cpu  100 0 50 800 40 5 5 0\ncpu0 50 0 25 400 20 2 3 0\n")
	busy, total, ok := parseCPU(b)
	if !ok {
		t.Fatal("parseCPU failed")
	}
	if total != 1000 {
		t.Fatalf("total = %d, want 1000", total)
	}
	if busy != 160 { // total - idle(800) - iowait(40)
		t.Fatalf("busy = %d, want 160", busy)
	}
	if _, _, ok := parseCPU([]byte("intr 12345\n")); ok {
		t.Fatal("no cpu line must not parse")
	}
}

func TestCollectorCPUFraction(t *testing.T) {
	c := New(t.TempDir())
	c.mu.Lock()
	c.prevBusy, c.prevTotal = 100, 1000
	c.mu.Unlock()
	// Simulate the next sample: +90 busy of +100 total.
	busy, total, _ := parseCPU([]byte("cpu  190 0 0 910 0 0 0 0\n"))
	c.mu.Lock()
	var cpu float32
	if c.prevTotal > 0 && total > c.prevTotal {
		cpu = float32(busy-c.prevBusy) / float32(total-c.prevTotal)
	}
	c.mu.Unlock()
	if math.Abs(float64(cpu)-0.9) > 1e-6 {
		t.Fatalf("cpu fraction = %v, want 0.9", cpu)
	}
}

func TestParseMem(t *testing.T) {
	b := []byte("MemTotal:       16384000 kB\nMemFree:         1000000 kB\nMemAvailable:    8192000 kB\n")
	total, used := parseMem(b)
	if total != 16384000<<10 {
		t.Fatalf("total = %d", total)
	}
	if used != (16384000-8192000)<<10 {
		t.Fatalf("used = %d", used)
	}
}

func TestParseLoad1(t *testing.T) {
	if v := parseLoad1([]byte("1.42 0.90 0.55 2/345 6789\n")); math.Abs(float64(v)-1.42) > 1e-6 {
		t.Fatalf("load1 = %v", v)
	}
	if v := parseLoad1(nil); v != 0 {
		t.Fatalf("empty loadavg: %v", v)
	}
}

// Sample must not panic anywhere (on non-Linux it returns zeros).
func TestSampleSmoke(t *testing.T) {
	c := New(t.TempDir())
	_ = c.Sample()
	_ = c.Sample()
}
