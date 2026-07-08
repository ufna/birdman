// Package imagegc frees the node disk by removing unused images
// (docs/specs/agent.md §6): data_dir disk above the watermark (80%) → delete
// least-recently-used images, protecting versions the master still cares
// about — the agent learns that set from PrePull/StartServer commands and
// keeps the last used refs (уточнено в v0: точных состояний версий агент не
// знает, прокси — последние использованные образы).
package imagegc

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Image is one image in the runtime store.
type Image struct {
	Name      string
	UpdatedAt time.Time
}

// Runtime is the containerd surface the GC needs.
type Runtime interface {
	// Images lists images in the birdman namespace.
	Images(ctx context.Context) ([]Image, error)
	// DeleteImage removes an image (and synchronously its content).
	DeleteImage(ctx context.Context, name string) error
	// UsedImageRefs returns refs referenced by existing containers.
	UsedImageRefs(ctx context.Context) (map[string]bool, error)
}

// protectedCap bounds the recently-used ref set: active + deprecated +
// prepulling per project on a node fit with room to spare.
const protectedCap = 8

// Options configure the GC.
type Options struct {
	Runtime   Runtime
	DiskUsage func() (used, total uint64) // data_dir filesystem
	Watermark float64                     // GC trigger, default 0.80
	Logf      func(string, ...any)
}

// GC is the background image collector. Touch() feeds the protected set.
type GC struct {
	rt        Runtime
	diskUsage func() (used, total uint64)
	watermark float64
	logf      func(string, ...any)

	mu      sync.Mutex
	touched map[string]time.Time // ref → last use
	now     func() time.Time
}

func New(o Options) *GC {
	if o.Watermark <= 0 {
		o.Watermark = 0.80
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return &GC{
		rt: o.Runtime, diskUsage: o.DiskUsage, watermark: o.Watermark, logf: o.Logf,
		touched: map[string]time.Time{}, now: time.Now,
	}
}

// Touch marks an image ref as recently used (StartServer/PrePull). The set is
// LRU-capped at protectedCap.
func (g *GC) Touch(ref string) {
	if ref == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.touched[ref] = g.now()
	if len(g.touched) <= protectedCap {
		return
	}
	oldestRef, oldestAt := "", time.Time{}
	for r, at := range g.touched {
		if oldestRef == "" || at.Before(oldestAt) {
			oldestRef, oldestAt = r, at
		}
	}
	delete(g.touched, oldestRef)
}

// Protected returns a copy of the protected ref set.
func (g *GC) Protected() map[string]bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]bool, len(g.touched))
	for r := range g.touched {
		out[r] = true
	}
	return out
}

// Run ticks RunOnce until ctx is done.
func (g *GC) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := g.RunOnce(ctx); err != nil && ctx.Err() == nil {
				g.logf("[imagegc] pass failed: %v", err)
			}
		}
	}
}

// RunOnce deletes unused images (oldest first) while the disk stays above the
// watermark. Returns the deleted refs.
func (g *GC) RunOnce(ctx context.Context) ([]string, error) {
	if !g.aboveWatermark() {
		return nil, nil
	}
	images, err := g.rt.Images(ctx)
	if err != nil {
		return nil, err
	}
	used, err := g.rt.UsedImageRefs(ctx)
	if err != nil {
		return nil, err
	}
	protected := g.Protected()

	candidates := images[:0]
	for _, img := range images {
		if used[img.Name] || protected[img.Name] {
			continue
		}
		candidates = append(candidates, img)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].UpdatedAt.Before(candidates[j].UpdatedAt)
	})

	var deleted []string
	for _, img := range candidates {
		if !g.aboveWatermark() {
			break
		}
		if err := g.rt.DeleteImage(ctx, img.Name); err != nil {
			g.logf("[imagegc] delete %s: %v", img.Name, err)
			continue
		}
		g.logf("[imagegc] deleted unused image %s", img.Name)
		deleted = append(deleted, img.Name)
	}
	if g.aboveWatermark() && len(candidates) == 0 {
		g.logf("[imagegc] disk above watermark but no removable images " +
			"(all in use or protected) — see runbook «Диск полон»")
	}
	return deleted, nil
}

func (g *GC) aboveWatermark() bool {
	used, total := g.diskUsage()
	if total == 0 {
		return false
	}
	return float64(used)/float64(total) > g.watermark
}
