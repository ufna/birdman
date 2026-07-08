package cgroups

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParsePath(t *testing.T) {
	in := []byte("12:pids:/legacy\n0::/birdman-slice/srv-1\n")
	p, err := ParsePath(in)
	if err != nil {
		t.Fatal(err)
	}
	if p != "/birdman-slice/srv-1" {
		t.Fatalf("path = %q", p)
	}
	if _, err := ParsePath([]byte("1:cpu:/only-v1\n")); err == nil {
		t.Fatal("expected error for cgroup v1-only content")
	}
	if _, err := ParsePath([]byte("0::relative\n")); err == nil {
		t.Fatal("expected error for a non-absolute path")
	}
}

func TestParseCPUStatUsec(t *testing.T) {
	usec, err := ParseCPUStatUsec([]byte("usage_usec 2500000\nuser_usec 2000000\nsystem_usec 500000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if usec != 2500000 {
		t.Fatalf("usage_usec = %d", usec)
	}
	if _, err := ParseCPUStatUsec([]byte("user_usec 1\n")); err == nil {
		t.Fatal("expected error without usage_usec")
	}
}

// TestRead exercises the full path resolution against a fake /proc +
// /sys/fs/cgroup layout (portable — no Linux needed).
func TestRead(t *testing.T) {
	root := t.TempDir()
	procDir := filepath.Join(root, "proc", "4242")
	cgDir := filepath.Join(root, "cgroup", "birdman", "srv-1")
	for _, d := range []string{procDir, cgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(procDir, "cgroup"), "0::/birdman/srv-1\n")
	writeFile(t, filepath.Join(cgDir, "cpu.stat"), "usage_usec 1500000\nuser_usec 1000000\n")
	writeFile(t, filepath.Join(cgDir, "memory.current"), "104857600\n")

	u, err := read(filepath.Join(root, "proc"), filepath.Join(root, "cgroup"), 4242)
	if err != nil {
		t.Fatal(err)
	}
	if u.CPUSeconds != 1.5 {
		t.Fatalf("cpu = %v", u.CPUSeconds)
	}
	if u.MemBytes != 104857600 {
		t.Fatalf("mem = %d", u.MemBytes)
	}

	if _, err := read(filepath.Join(root, "proc"), filepath.Join(root, "cgroup"), 999); err == nil {
		t.Fatal("expected error for unknown pid")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
