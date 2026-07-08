package logrot

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gzipContent(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRotateOnceCopyTruncate(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, MaxSize: 10, Logf: t.Logf}, func() []string { return []string{"srv"} })

	path := filepath.Join(dir, "srv.log")
	write(t, path, "0123456789ABCDEF") // 16 bytes > 10

	// Simulate the shim's open fd: it must keep writing into the same inode.
	shim, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer shim.Close()

	r.RotateOnce()

	if got, _ := os.ReadFile(path + ".1"); string(got) != "0123456789ABCDEF" {
		t.Fatalf("rotated copy = %q", got)
	}
	if st, err := os.Stat(path); err != nil || st.Size() != 0 {
		t.Fatalf("active log not truncated: %v %d", err, st.Size())
	}
	if _, err := shim.WriteString("new\n"); err != nil {
		t.Fatal(err)
	}
	// O_APPEND: writes land at the (new) end of the truncated file.
	if got, _ := os.ReadFile(path); !strings.HasSuffix(string(got), "new\n") {
		t.Fatalf("shim write lost after rotation: %q", got)
	}

	// Below threshold — second rotation is a no-op.
	r.RotateOnce()
	if got, _ := os.ReadFile(path + ".1"); string(got) != "0123456789ABCDEF" {
		t.Fatalf(".1 overwritten by a no-op rotation: %q", got)
	}
}

func TestFinalizeAndRetention(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, Retention: time.Hour, SweepEvery: time.Minute, Logf: t.Logf},
		func() []string { return []string{"live"} })

	write(t, filepath.Join(dir, "dead.log"), "tail part\n")
	write(t, filepath.Join(dir, "dead.log.1"), "head part\n")
	r.Finalize("dead")

	for _, name := range []string{"dead.log", "dead.log.1"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s not removed after finalize", name)
		}
	}
	if got := gzipContent(t, filepath.Join(dir, "dead.log.gz")); got != "tail part\n" {
		t.Fatalf("dead.log.gz = %q", got)
	}
	if got := gzipContent(t, filepath.Join(dir, "dead.log.1.gz")); got != "head part\n" {
		t.Fatalf("dead.log.1.gz = %q", got)
	}

	// Retention: age one archive beyond the cutoff.
	old := filepath.Join(dir, "dead.log.1.gz")
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	// A live server's log is never touched by the sweep.
	write(t, filepath.Join(dir, "live.log"), "live\n")
	livePast := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "live.log"), livePast, livePast); err != nil {
		t.Fatal(err)
	}

	r.SweepOnce()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired archive not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "dead.log.gz")); err != nil {
		t.Fatal("fresh archive removed prematurely")
	}
	if _, err := os.Stat(filepath.Join(dir, "live.log")); err != nil {
		t.Fatal("live log must not be touched")
	}

	// A stale plain log of a non-live server gets gzipped by the sweep.
	write(t, filepath.Join(dir, "orphan.log"), "orphan\n")
	if err := os.Chtimes(filepath.Join(dir, "orphan.log"), past, past); err != nil {
		t.Fatal(err)
	}
	r.SweepOnce()
	if got := gzipContent(t, filepath.Join(dir, "orphan.log.gz")); got != "orphan\n" {
		t.Fatalf("orphan.log.gz = %q", got)
	}
}

func collect(t *testing.T, dir, id string, tailLines int) string {
	t.Helper()
	var buf bytes.Buffer
	err := Stream(context.Background(), dir, id, tailLines, false, func(b []byte) error {
		buf.Write(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestStreamColdParts(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "srv.log.1"), "one\ntwo\n")
	write(t, filepath.Join(dir, "srv.log"), "three\nfour\n")

	if got := collect(t, dir, "srv", 0); got != "one\ntwo\nthree\nfour\n" {
		t.Fatalf("full stream = %q", got)
	}
	if got := collect(t, dir, "srv", 3); got != "two\nthree\nfour\n" {
		t.Fatalf("tail 3 = %q", got)
	}

	// Finalized (gz) parts read the same — the reaped-server download path.
	r := New(Config{Dir: dir, Logf: t.Logf}, nil)
	r.Finalize("srv")
	if got := collect(t, dir, "srv", 0); got != "one\ntwo\nthree\nfour\n" {
		t.Fatalf("gz stream = %q", got)
	}

	if err := Stream(context.Background(), dir, "ghost", 0, false, func([]byte) error { return nil }); err != ErrNoLogs {
		t.Fatalf("ghost err = %v", err)
	}
}

func TestStreamFollow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "srv.log")
	write(t, path, "start\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chunks := make(chan string, 64)
	done := make(chan error, 1)
	go func() {
		done <- Stream(ctx, dir, "srv", 0, true, func(b []byte) error {
			chunks <- string(b)
			return nil
		})
	}()

	expect := func(want string) {
		t.Helper()
		var got string
		deadline := time.After(5 * time.Second)
		for !strings.Contains(got, want) {
			select {
			case c := <-chunks:
				got += c
			case <-deadline:
				t.Fatalf("did not see %q, got %q", want, got)
			}
		}
	}
	expect("start\n")

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f, "appended\n")
	f.Close()
	expect("appended\n")

	// Finalize (server stopped): follow must end cleanly with nil.
	r := New(Config{Dir: dir, Logf: t.Logf}, nil)
	r.Finalize("srv")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("follow end: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not end after finalize")
	}
}

func TestStreamFollowRotationReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "srv.log")
	write(t, path, "aaaa\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu bytes.Buffer
	sync := make(chan struct{}, 64)
	go Stream(ctx, dir, "srv", 0, true, func(b []byte) error {
		mu.Write(b)
		sync <- struct{}{}
		return nil
	})

	wait := func() {
		select {
		case <-sync:
		case <-time.After(5 * time.Second):
			t.Fatal("no chunk")
		}
	}
	wait() // backlog

	// copy-truncate rotation, then new content shorter than the old offset.
	r := New(Config{Dir: dir, MaxSize: 1, Logf: t.Logf}, func() []string { return []string{"srv"} })
	r.RotateOnce()
	write(t, path, "bb\n")
	wait()
	if got := mu.String(); !strings.Contains(got, "bb\n") {
		t.Fatalf("post-rotation content lost: %q", got)
	}
}
