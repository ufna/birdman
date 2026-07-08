package qosecho

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

func TestEcho(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Reserve an address, free it, serve on it.
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.LocalAddr().String()
	probe.Close()

	done := make(chan error, 1)
	go func() { done <- Serve(ctx, addr, t.Logf) }()

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	send := func(payload []byte) []byte {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		buf := make([]byte, 2048)
		for time.Now().Before(deadline) {
			if _, err := conn.Write(payload); err != nil {
				t.Fatal(err)
			}
			conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, err := conn.Read(buf)
			if err == nil {
				return buf[:n]
			}
		}
		t.Fatal("no echo before deadline")
		return nil
	}

	// Small payload comes back verbatim.
	if got := send([]byte("ping-1")); !bytes.Equal(got, []byte("ping-1")) {
		t.Fatalf("echo = %q", got)
	}
	// Oversized payload is capped at MaxEcho bytes.
	big := bytes.Repeat([]byte("x"), 500)
	if got := send(big); !bytes.Equal(got, big[:MaxEcho]) {
		t.Fatalf("oversized echo: %d bytes", len(got))
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}
