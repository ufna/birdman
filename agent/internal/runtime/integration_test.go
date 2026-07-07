//go:build integration

package runtime

// Интеграция с настоящим containerd — запускается вручную на Linux-тачке
// с демоном (docker 28.x → containerd 1.7.x):
//
//	cd agent && go test -race -tags integration ./internal/runtime/
//
// Env-переопределения:
//
//	BIRDMAN_TEST_CONTAINERD — сокет демона (default /run/containerd/containerd.sock)
//	BIRDMAN_TEST_IMAGE      — публичный образ (default docker.io/library/busybox:1.36)
//	BIRDMAN_TEST_SOCKDIR    — каталог для liba-сокета (default t.TempDir()); нужен,
//	                          когда тест и демон в разных mount-namespace (клиент в
//	                          контейнере): источник bind-mount обязан быть виден демону

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerRoundtrip(t *testing.T) {
	addr := os.Getenv("BIRDMAN_TEST_CONTAINERD")
	if addr == "" {
		addr = DefaultAddress
	}
	ref := os.Getenv("BIRDMAN_TEST_IMAGE")
	if ref == "" {
		ref = "docker.io/library/busybox:1.36"
	}

	client, err := Connect(addr)
	if err != nil {
		t.Fatalf("containerd daemon required for -tags integration: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	img, err := client.EnsureImage(ctx, ref, nil)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	sockDir := os.Getenv("BIRDMAN_TEST_SOCKDIR")
	if sockDir == "" {
		sockDir = dir
	}
	// Per-server dir layout: {sockDir}/{id}/agent.sock — the dir is what
	// gets bind-mounted (ro) into the container.
	id := fmt.Sprintf("itest-%d", time.Now().UnixNano())
	serverSockDir := filepath.Join(sockDir, id)
	if err := os.MkdirAll(serverSockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(serverSockDir)
	sockPath := filepath.Join(serverSockDir, SocketFileName)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	logPath := filepath.Join(dir, "server.log")
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer logF.Close()

	srv, err := client.StartServer(ctx, ServerSpec{
		ID:         id,
		Image:      img,
		ImageRef:   ref,
		Port:       20999,
		Region:     "test",
		SocketPath: sockPath,
		CPUMillis:  500,
		MemMB:      128,
		Args: []string{"sh", "-c",
			"echo integration-ok; test -S /birdman/agent.sock && echo sock-ok; env | sort"},
	}, logF)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case st := <-srv.Wait():
		code, _, werr := st.Result()
		if werr != nil {
			t.Fatal(werr)
		}
		if code != 0 {
			t.Fatalf("container exit code %d", code)
		}
	case <-time.After(time.Minute):
		_ = srv.ForceKill(ctx)
		t.Fatal("container did not exit")
	}
	if err := srv.Delete(ctx); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, want := range []string{
		"integration-ok",
		"sock-ok",
		"BIRDMAN_PORT=20999",
		"BIRDMAN_SERVER_ID=" + id,
		"BIRDMAN_SOCKET=" + ContainerSocketPath,
		"BIRDMAN_REGION=test",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("server log missing %q; log:\n%s", want, out)
		}
	}
}
