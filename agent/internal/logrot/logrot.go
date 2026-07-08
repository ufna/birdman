// Package logrot owns the dedik log files (docs/specs/agent.md §5):
//
//   - rotation of active logs: 100MB × 2 files per server ({id}.log +
//     {id}.log.1). The containerd shim keeps an open fd on {id}.log, so
//     rotation is copy-truncate — the shim keeps writing into the same inode;
//   - gzip after stop: Finalize({id}) compresses both files once the
//     container is gone;
//   - retention: archives (and stray logs of servers no longer supervised)
//     older than N days are removed by the background sweep.
package logrot

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config parameterizes the rotator. Zero values get spec defaults.
type Config struct {
	Dir         string        // server log dir ({log_dir}/servers)
	MaxSize     int64         // rotation threshold, default 100MB (spec §5)
	Retention   time.Duration // archive retention, default 7 days (spec §5)
	SweepEvery  time.Duration // background pass period, default 1m
	Logf        func(string, ...any)
}

// Rotator runs rotation + retention for one log dir. live() reports the ids
// of currently supervised servers: their logs rotate; everything else is
// subject to finalize/retention.
type Rotator struct {
	cfg  Config
	live func() []string
}

func New(cfg Config, live func() []string) *Rotator {
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = 100 << 20
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 7 * 24 * time.Hour
	}
	if cfg.SweepEvery <= 0 {
		cfg.SweepEvery = time.Minute
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if live == nil {
		live = func() []string { return nil }
	}
	return &Rotator{cfg: cfg, live: live}
}

// Run ticks RotateOnce+SweepOnce until the channel is closed/ctx done.
func (r *Rotator) Run(done <-chan struct{}) {
	t := time.NewTicker(r.cfg.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			r.RotateOnce()
			r.SweepOnce()
		}
	}
}

// RotateOnce copy-truncates every live server log above MaxSize.
func (r *Rotator) RotateOnce() {
	for _, id := range r.live() {
		path := filepath.Join(r.cfg.Dir, id+".log")
		st, err := os.Stat(path)
		if err != nil || st.Size() < r.cfg.MaxSize {
			continue
		}
		if err := copyTruncate(path, path+".1"); err != nil {
			r.cfg.Logf("[logrot] rotate %s: %v", id, err)
			continue
		}
		r.cfg.Logf("[logrot] rotated %s (%d bytes)", id, st.Size())
	}
}

// Finalize gzips the log files of a stopped server ({id}.log{,.1} → .gz).
// Call it after the container is deleted — the shim no longer writes.
func (r *Rotator) Finalize(id string) {
	for _, name := range []string{id + ".log.1", id + ".log"} {
		path := filepath.Join(r.cfg.Dir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := gzipFile(path); err != nil {
			r.cfg.Logf("[logrot] gzip %s: %v", name, err)
			continue
		}
	}
}

// SweepOnce enforces retention and finalizes logs of servers that are no
// longer supervised (safety net for finalizes missed across agent restarts;
// grace of one sweep period avoids racing a starting server).
func (r *Rotator) SweepOnce() {
	entries, err := os.ReadDir(r.cfg.Dir)
	if err != nil {
		r.cfg.Logf("[logrot] sweep: %v", err)
		return
	}
	live := map[string]bool{}
	for _, id := range r.live() {
		live[id] = true
	}
	cutoff := time.Now().Add(-r.cfg.Retention)
	staleCutoff := time.Now().Add(-2 * r.cfg.SweepEvery)

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		path := filepath.Join(r.cfg.Dir, name)
		info, err := e.Info()
		if err != nil {
			continue
		}
		switch {
		case strings.HasSuffix(name, ".gz"):
			if info.ModTime().Before(cutoff) {
				if err := os.Remove(path); err != nil {
					r.cfg.Logf("[logrot] retention %s: %v", name, err)
				} else {
					r.cfg.Logf("[logrot] retention: removed %s", name)
				}
			}
		case strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".log.1"):
			id := strings.TrimSuffix(strings.TrimSuffix(name, ".1"), ".log")
			if live[id] {
				continue
			}
			// Not supervised and not written for a while — the server is
			// gone; compress like Finalize would have.
			if info.ModTime().Before(staleCutoff) {
				if err := gzipFile(path); err != nil {
					r.cfg.Logf("[logrot] sweep gzip %s: %v", name, err)
				} else {
					r.cfg.Logf("[logrot] sweep: gzipped stale %s", name)
				}
			}
		}
	}
}

// copyTruncate copies src to dst (replacing it) and truncates src to zero —
// writers with an open fd keep appending to the same inode.
func copyTruncate(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Truncate(src, 0)
}

// gzipFile compresses path to path+".gz" (replacing it) and removes path.
func gzipFile(path string) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(path+".gz", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		out.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(path)
}
