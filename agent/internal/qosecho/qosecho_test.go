package qosecho

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
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

// Эхо — ресурс ХОСТА, и держит его тот агент бокса, кто жив (tracker #1068).
// Проигравший гонку не умирает и не молчит навсегда: он ждёт освобождения
// порта и подхватывает его. Без этого смерть агента-ВЛАДЕЛЬЦА гасила бы
// ping-таргет бокса и для СОСЕДНЕГО проекта, чью ноду мастер продолжает
// отдавать в GET /v1/qos, пока её собственный агент жив.
func TestEchoContention(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Сосед по боксу выиграл гонку старта и держит порт (сам он на пробы не
	// отвечает — это просто занятый сокет).
	holder, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := holder.LocalAddr().String()

	var mu sync.Mutex
	var lines []string
	logf := func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(f, a...)))
	}
	countBusy := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, l := range lines {
			if strings.Contains(l, "held by another agent") {
				n++
			}
		}
		return n
	}

	done := make(chan error, 1)
	go func() { done <- ServeWithRetry(ctx, addr, 20*time.Millisecond, logf) }()

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	echo := func(timeout time.Duration) bool {
		t.Helper()
		deadline := time.Now().Add(timeout)
		buf := make([]byte, 64)
		for time.Now().Before(deadline) {
			if _, err := conn.Write([]byte("probe")); err != nil {
				return false
			}
			conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			if n, err := conn.Read(buf); err == nil && bytes.Equal(buf[:n], []byte("probe")) {
				return true
			}
		}
		return false
	}

	// Порт занят соседом — этот агент не отвечает (и не падает).
	if echo(300 * time.Millisecond) {
		t.Fatal("ответ пришёл, пока порт держит сосед — второй bind на том же порту невозможен")
	}
	// Занятость сказана ОДИН раз на переход, а не на каждый тик ретрая:
	// за ~300мс при ретрае 20мс попыток было полтора десятка.
	if n := countBusy(); n != 1 {
		t.Fatalf("строк «порт занят» = %d, ожидалась ровно одна на переход состояния", n)
	}

	// Агент-владелец лёг (юнит остановлен, краш-луп, вывод ноды) — порт
	// освободился, и его обязан подхватить любой живой агент бокса.
	holder.Close()
	if !echo(5 * time.Second) {
		t.Fatal("порт освободился, а эхо никто не подхватил — таргет бокса остался тёмным")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}
