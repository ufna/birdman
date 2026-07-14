package imagegc

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

type fakeRuntime struct {
	mu     sync.Mutex
	images []Image
	used   map[string]bool
	// freedPerDelete drops disk usage on every delete (simulates freed space).
	freed uint64
	disk  *fakeDisk
}

type fakeDisk struct {
	mu          sync.Mutex
	used, total uint64
}

func (d *fakeDisk) usage() (uint64, uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.used, d.total
}

func (r *fakeRuntime) Images(context.Context) ([]Image, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.images), nil
}

func (r *fakeRuntime) UsedImageRefs(context.Context) (map[string]bool, error) {
	return r.used, nil
}

func (r *fakeRuntime) DeleteImage(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, img := range r.images {
		if img.Name == name {
			r.images = slices.Delete(r.images, i, i+1)
			r.disk.mu.Lock()
			r.disk.used -= r.freed
			r.disk.mu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("image %s not found", name)
}

func at(h int) time.Time { return time.Date(2026, 7, 1, h, 0, 0, 0, time.UTC) }

func TestRunOnceBelowWatermarkNoop(t *testing.T) {
	disk := &fakeDisk{used: 10, total: 100}
	rt := &fakeRuntime{images: []Image{{Name: "img:1", UpdatedAt: at(1)}}, used: map[string]bool{}, disk: disk}
	g := New(Options{Runtime: rt, DiskUsage: disk.usage, Logf: t.Logf})
	deleted, err := g.RunOnce(context.Background())
	if err != nil || deleted != nil {
		t.Fatalf("below watermark: %v %v", deleted, err)
	}
}

func TestRunOnceDeletesLRUUntilBelow(t *testing.T) {
	// 3 deletable images; each delete frees 5 of 100 → 92 → 87 → below 90? no:
	// watermark 0.80 → need to reach ≤80: 92→87→82→ stop at 77 after third.
	disk := &fakeDisk{used: 92, total: 100}
	rt := &fakeRuntime{
		images: []Image{
			{Name: "old:1", UpdatedAt: at(1)},
			{Name: "mid:2", UpdatedAt: at(2)},
			{Name: "new:3", UpdatedAt: at(3)},
			{Name: "used:4", UpdatedAt: at(0)},
			{Name: "protected:5", UpdatedAt: at(0)},
		},
		used:  map[string]bool{"used:4": true},
		freed: 5,
		disk:  disk,
	}
	g := New(Options{Runtime: rt, DiskUsage: disk.usage, Watermark: 0.80, Logf: t.Logf})
	g.Touch("protected:5")

	deleted, err := g.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 92 → 87 (old) → 82 (mid) → 77 (new) ≤ 80 → stop.
	want := []string{"old:1", "mid:2", "new:3"}
	if !slices.Equal(deleted, want) {
		t.Fatalf("deleted %v, want %v (LRU order, in-use and protected skipped)", deleted, want)
	}
	for _, img := range rt.images {
		if img.Name == "used:4" || img.Name == "protected:5" {
			continue
		}
		t.Fatalf("unexpected survivor %s", img.Name)
	}
}

func TestRunOnceStopsOnceBelow(t *testing.T) {
	disk := &fakeDisk{used: 85, total: 100}
	rt := &fakeRuntime{
		images: []Image{
			{Name: "a:1", UpdatedAt: at(1)},
			{Name: "b:2", UpdatedAt: at(2)},
		},
		used:  map[string]bool{},
		freed: 10, // one delete brings 85 → 75 < 80
		disk:  disk,
	}
	g := New(Options{Runtime: rt, DiskUsage: disk.usage, Watermark: 0.80, Logf: t.Logf})
	deleted, err := g.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{"a:1"}) {
		t.Fatalf("deleted %v, want only the oldest", deleted)
	}
}

// TestUntouchDropsRef covers the RemoveImage side of the protected set
// (environments v1 §6б, РЕВИЗИЯ M12): a retired image's ref must leave the
// LRU-touched set so a dead ref no longer occupies a protection slot. Untouch
// of an unknown or empty ref is a harmless no-op.
func TestUntouchDropsRef(t *testing.T) {
	g := New(Options{Runtime: nil, DiskUsage: func() (uint64, uint64) { return 0, 0 }, Logf: t.Logf})
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	i := 0
	g.now = func() time.Time { i++; return base.Add(time.Duration(i) * time.Second) }

	g.Touch("img:1") // oldest
	g.Touch("img:2")
	if !g.Protected()["img:1"] {
		t.Fatal("img:1 must be protected after Touch")
	}

	g.Untouch("img:1")
	if g.Protected()["img:1"] {
		t.Fatal("img:1 must be gone from the protected set after Untouch")
	}
	if !g.Protected()["img:2"] {
		t.Fatal("Untouch of img:1 must not affect img:2")
	}

	// Untouch of an unknown or empty ref is a harmless no-op.
	g.Untouch("nope")
	g.Untouch("")
	if !g.Protected()["img:2"] || len(g.Protected()) != 1 {
		t.Fatalf("no-op Untouch must not change the set, got %v", g.Protected())
	}

	// A dropped ref actually frees a cap slot. Fill the set to the cap (img:2,
	// touched first, is the oldest), then Untouch one entry and Touch a fresh
	// ref: the freed slot means the newcomer fits WITHOUT evicting the oldest
	// (img:2). At a full cap that same Touch would evict img:2 (TestTouchLRUCap).
	for n := range protectedCap - 1 { // + img:2 already present = protectedCap
		g.Touch(fmt.Sprintf("fill:%d", n))
	}
	if len(g.Protected()) != protectedCap {
		t.Fatalf("precondition: set must be full at cap %d, got %d", protectedCap, len(g.Protected()))
	}
	g.Untouch("fill:3") // free one slot (size → cap-1)
	g.Touch("fresh:1")  // lands in the freed slot, no eviction
	p := g.Protected()
	if !p["img:2"] {
		t.Fatal("oldest ref must survive: the Untouch freed the slot the fresh Touch used")
	}
	if !p["fresh:1"] {
		t.Fatal("fresh ref must be protected")
	}
	if p["fill:3"] {
		t.Fatal("the untouched ref must be gone")
	}
	if len(p) != protectedCap {
		t.Fatalf("set must be back at exactly the cap, got %d", len(p))
	}
}

// TestDualFsWatermarkContainerdTriggers covers the dual-fs watermark
// (environments v1 §6в): images live on the containerd root, which can be a
// separate mount from data_dir. When containerd is above the watermark the GC
// must run even though data_dir is far below it — aboveWatermark takes the
// MAX usage of the two filesystems.
func TestDualFsWatermarkContainerdTriggers(t *testing.T) {
	dataDisk := &fakeDisk{used: 10, total: 100} // data_dir far below watermark
	cdDisk := &fakeDisk{used: 92, total: 100}   // containerd root above 80%
	rt := &fakeRuntime{
		images: []Image{{Name: "old:1", UpdatedAt: at(1)}},
		used:   map[string]bool{},
		disk:   dataDisk, // fake frees data_dir on delete; irrelevant here (freed=0)
	}
	g := New(Options{
		Runtime:             rt,
		DiskUsage:           dataDisk.usage,
		ContainerdDiskUsage: cdDisk.usage,
		Watermark:           0.80,
		Logf:                t.Logf,
	})
	deleted, err := g.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{"old:1"}) {
		t.Fatalf("containerd fs above watermark must trigger GC even with data_dir low, deleted %v", deleted)
	}
}

// TestDualFsWatermarkDataDirStillTriggers is the symmetric case: data_dir
// above the watermark while containerd is low still runs the GC — the MAX of
// the two drives the decision, not the containerd fs alone.
func TestDualFsWatermarkDataDirStillTriggers(t *testing.T) {
	dataDisk := &fakeDisk{used: 92, total: 100} // data_dir above 80%
	cdDisk := &fakeDisk{used: 10, total: 100}   // containerd root far below
	rt := &fakeRuntime{
		images: []Image{{Name: "old:1", UpdatedAt: at(1)}},
		used:   map[string]bool{},
		freed:  20, // one delete drops data_dir 92 → 72 < 80 → stop
		disk:   dataDisk,
	}
	g := New(Options{
		Runtime:             rt,
		DiskUsage:           dataDisk.usage,
		ContainerdDiskUsage: cdDisk.usage,
		Watermark:           0.80,
		Logf:                t.Logf,
	})
	deleted, err := g.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{"old:1"}) {
		t.Fatalf("data_dir above watermark must still trigger GC, deleted %v", deleted)
	}
}

// TestDualFsWatermarkBothBelowNoop: with both filesystems below the watermark
// the GC stays idle (a nil ContainerdDiskUsage must not change the single-fs
// behavior either — covered by the other RunOnce tests that omit it).
func TestDualFsWatermarkBothBelowNoop(t *testing.T) {
	dataDisk := &fakeDisk{used: 10, total: 100}
	cdDisk := &fakeDisk{used: 50, total: 100}
	rt := &fakeRuntime{images: []Image{{Name: "old:1", UpdatedAt: at(1)}}, used: map[string]bool{}, disk: dataDisk}
	g := New(Options{Runtime: rt, DiskUsage: dataDisk.usage, ContainerdDiskUsage: cdDisk.usage, Watermark: 0.80, Logf: t.Logf})
	deleted, err := g.RunOnce(context.Background())
	if err != nil || deleted != nil {
		t.Fatalf("both filesystems below watermark: deleted %v err %v", deleted, err)
	}
}

func TestTouchLRUCap(t *testing.T) {
	g := New(Options{Runtime: nil, DiskUsage: func() (uint64, uint64) { return 0, 0 }, Logf: t.Logf})
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	i := 0
	g.now = func() time.Time { i++; return base.Add(time.Duration(i) * time.Second) }
	for n := range protectedCap + 3 {
		g.Touch(fmt.Sprintf("img:%d", n))
	}
	p := g.Protected()
	if len(p) != protectedCap {
		t.Fatalf("protected size %d, want %d", len(p), protectedCap)
	}
	for n := range 3 {
		if p[fmt.Sprintf("img:%d", n)] {
			t.Fatalf("oldest ref img:%d must be evicted", n)
		}
	}
	if !p[fmt.Sprintf("img:%d", protectedCap+2)] {
		t.Fatal("newest ref must be protected")
	}
}
