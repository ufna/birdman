//go:build linux

package uds

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// Use a subprocess because umask is process-global and must not affect SDK
// transport tests that create sockets while their reader goroutines are live.
func TestSDKSocketDirectorySurvivesRestrictiveUmask(t *testing.T) {
	if os.Getenv("BIRDMAN_TEST_SOCKET_UMASK") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestSDKSocketDirectorySurvivesRestrictiveUmask$", "-test.v")
		command.Env = append(os.Environ(), "BIRDMAN_TEST_SOCKET_UMASK=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("restrictive umask subprocess: %v\n%s", err, output)
		}
		return
	}
	syscall.Umask(0o077)
	private, err := os.MkdirTemp("", "sdk-mode-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(private)
	shared := filepath.Join(private, "dedicated")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	assertMode := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("expected mode %04o for %s: info=%v err=%v", want, path, info, err)
		}
	}
	assertMode(shared, 0o700) // Reproduce systemd UMask=0077.
	path := filepath.Join(shared, "agent.sock")
	server, err := Listen(path, Events{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	assertMode(shared, 0o755)
	assertMode(path, 0o666)
	assertMode(private, 0o700)
}
