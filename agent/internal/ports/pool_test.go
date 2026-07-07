package ports

import (
	"errors"
	"sync"
	"testing"
)

func TestAcquireLowestFree(t *testing.T) {
	p, err := New(20000, 20002)
	if err != nil {
		t.Fatal(err)
	}
	for want := 20000; want <= 20002; want++ {
		got, err := p.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("Acquire = %d, want %d", got, want)
		}
	}
	if _, err := p.Acquire(); !errors.Is(err, ErrExhausted) {
		t.Fatalf("want ErrExhausted, got %v", err)
	}
	if err := p.Release(20001); err != nil {
		t.Fatal(err)
	}
	got, err := p.Acquire()
	if err != nil || got != 20001 {
		t.Fatalf("reacquire = %d, %v", got, err)
	}
}

func TestAcquireSpecific(t *testing.T) {
	p, err := New(20000, 20010)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AcquireSpecific(20005); err != nil {
		t.Fatal(err)
	}
	if err := p.AcquireSpecific(20005); err == nil {
		t.Fatal("double specific acquire must fail")
	}
	if err := p.AcquireSpecific(19999); err == nil {
		t.Fatal("acquire below range must fail")
	}
	if err := p.AcquireSpecific(20011); err == nil {
		t.Fatal("acquire above range must fail")
	}
	got, err := p.Acquire()
	if err != nil || got != 20000 {
		t.Fatalf("Acquire = %d, %v", got, err)
	}
}

func TestReleaseErrors(t *testing.T) {
	p, err := New(20000, 20001)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Release(20000); err == nil {
		t.Fatal("release of a free port must fail")
	}
	if err := p.Release(30000); err == nil {
		t.Fatal("release out of range must fail")
	}
	if err := p.AcquireSpecific(20000); err != nil {
		t.Fatal(err)
	}
	if err := p.Release(20000); err != nil {
		t.Fatal(err)
	}
	if err := p.Release(20000); err == nil {
		t.Fatal("double release must fail")
	}
}

func TestNewValidation(t *testing.T) {
	for _, tc := range [][2]int{{0, 100}, {-1, 5}, {100, 99}, {1, 70000}} {
		if _, err := New(tc[0], tc[1]); err == nil {
			t.Fatalf("New(%d, %d) must fail", tc[0], tc[1])
		}
	}
}

func TestConcurrentAcquireRelease(t *testing.T) {
	p, err := New(20000, 20099)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port, err := p.Acquire()
			if err != nil {
				t.Error(err)
				return
			}
			if err := p.Release(port); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := p.InUse(); n != 0 {
		t.Fatalf("InUse = %d, want 0", n)
	}
}
