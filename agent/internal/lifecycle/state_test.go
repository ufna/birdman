package lifecycle

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestHappyPath(t *testing.T) {
	var got []Transition
	m := NewMachine("srv-1", func(tr Transition) { got = append(got, tr) })
	if m.Current() != StatePulling {
		t.Fatalf("initial state = %s", m.Current())
	}
	steps := []State{StateStarting, StateReady, StateAllocated, StateDraining, StateStopped}
	for _, s := range steps {
		if err := m.To(s, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if m.Current() != StateStopped {
		t.Fatal(m.Current())
	}
	if len(got) != len(steps) {
		t.Fatalf("logged %d transitions, want %d", len(got), len(steps))
	}
	if got[0].From != StatePulling || got[0].To != StateStarting || got[0].ServerID != "srv-1" {
		t.Fatalf("first transition: %+v", got[0])
	}
}

func TestInvalidTransitions(t *testing.T) {
	m := NewMachine("srv-2", nil)
	for _, s := range []State{StateReady, StateAllocated, StateDraining} {
		if err := m.To(s, ""); err == nil {
			t.Fatalf("pulling -> %s must fail", s)
		}
	}
	if err := m.To(StateStarting, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.To(StateReady, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.To(StateReady, ""); err == nil {
		t.Fatal("ready -> ready must fail")
	}
	if err := m.To(StateStopped, ""); err != nil {
		t.Fatal(err)
	}
	for _, s := range []State{StatePulling, StateStarting, StateReady, StateAllocated, StateDraining, StateFailed} {
		if err := m.To(s, ""); err == nil {
			t.Fatalf("stopped -> %s must fail (terminal)", s)
		}
	}
}

func TestTerminal(t *testing.T) {
	for _, s := range []State{StatePulling, StateStarting, StateReady, StateAllocated, StateDraining} {
		if s.Terminal() {
			t.Fatalf("%s must not be terminal", s)
		}
	}
	if !StateStopped.Terminal() || !StateFailed.Terminal() {
		t.Fatal("stopped and failed must be terminal")
	}
}

func TestSubscribe(t *testing.T) {
	m := NewMachine("srv-3", nil)
	sub := m.Subscribe()
	if err := m.To(StateStarting, "boot"); err != nil {
		t.Fatal(err)
	}
	select {
	case tr := <-sub:
		if tr.To != StateStarting || tr.Reason != "boot" {
			t.Fatalf("%+v", tr)
		}
	case <-time.After(time.Second):
		t.Fatal("no transition received")
	}
}

// Grace-таймаут: сервер не прислал ready — watcher срабатывает.
func TestWatchReadyGraceExpires(t *testing.T) {
	m := NewMachine("srv-4", nil)
	if err := m.To(StateStarting, ""); err != nil {
		t.Fatal(err)
	}
	expired := make(chan struct{})
	go WatchReadyGrace(context.Background(), m, 30*time.Millisecond, func() { close(expired) })
	select {
	case <-expired:
	case <-time.After(2 * time.Second):
		t.Fatal("grace did not expire")
	}
}

// ready до истечения grace — watcher не срабатывает и завершается.
func TestWatchReadyGraceCancelledByReady(t *testing.T) {
	m := NewMachine("srv-5", nil)
	if err := m.To(StateStarting, ""); err != nil {
		t.Fatal(err)
	}
	expired := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		WatchReadyGrace(context.Background(), m, 400*time.Millisecond, func() { expired <- struct{}{} })
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	if err := m.To(StateReady, "liba ready"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not return after leaving starting")
	}
	select {
	case <-expired:
		t.Fatal("grace fired after ready")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestWatchReadyGraceReturnsWhenNotStarting(t *testing.T) {
	m := NewMachine("srv-6", nil) // still pulling
	done := make(chan struct{})
	go func() {
		WatchReadyGrace(context.Background(), m, time.Hour, func() { t.Error("must not fire") })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher must return immediately when not in starting")
	}
}

func TestWatchReadyGraceCtxCancel(t *testing.T) {
	m := NewMachine("srv-7", nil)
	if err := m.To(StateStarting, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		WatchReadyGrace(ctx, m, time.Hour, func() { t.Error("must not fire") })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher must return on ctx cancel")
	}
}

func TestConcurrentTransitions(t *testing.T) {
	m := NewMachine("srv-8", nil)
	if err := m.To(StateStarting, ""); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.To(StateReady, "race") // ровно один выигрывает
		}()
	}
	wg.Wait()
	if m.Current() != StateReady {
		t.Fatal(m.Current())
	}
}

// NewMachineAt restores a machine mid-lifecycle (agent restart, map from
// containerd labels): transitions continue from the restored state.
func TestNewMachineAt(t *testing.T) {
	m := NewMachineAt("srv-2", StateReady, nil)
	if m.Current() != StateReady {
		t.Fatalf("restored state = %s", m.Current())
	}
	if err := m.To(StateAllocated, "master allocated"); err != nil {
		t.Fatal(err)
	}
	if err := m.To(StateStarting, "no way back"); err == nil {
		t.Fatal("illegal transition from restored chain must fail")
	}

	d := NewMachineAt("srv-3", StateDraining, nil)
	if err := d.To(StateStopped, "drained"); err != nil {
		t.Fatal(err)
	}
	if !d.Current().Terminal() {
		t.Fatal("stopped must be terminal")
	}
}
