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

// ServerDir resolves the directory holding the log files of one server.
//
// Since tracker #994 the agent writes a dedik's output into
// {root}/{project}/{env}/{id}.log: vector parses the pair out of the PATH and
// labels the VictoriaLogs stream with it, so master can narrow a bound key's
// LogsQL to its own project (docs/specs/master.md §6). The pair must ride with
// the DEDIK, not with the host — a node's env changes through the master API
// without re-rendering any node config (invariant I6), so a static per-host
// label would drift.
//
// The pair is deliberately NOT kept in agent memory: after an agent restart
// the server map is rebuilt from container labels, which do not carry it. The
// directory is resolved from the filesystem instead — the path IS where the
// pair lives. Falls back to root, which covers both logs written before the
// upgrade (they stay flat and unlabeled — a bound key does not get them, see
// the spec) and run-once mode, where there is no fleet and no pair.
func ServerDir(root, id string) string {
	// id приходит от master'а и подставляется в ГЛОБ. Метасимвол в нём заставил
	// бы резолвер найти чужой файл, а разделитель пути — выйти из дерева; такой
	// id не резолвим вовсе (плоский фоллбэк), а не резолвим «как получится».
	if id == "" || strings.ContainsAny(id, `*?[]\/`) {
		return root
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", "*", id+".log*"))
	if err == nil && len(matches) > 0 {
		return filepath.Dir(matches[0])
	}
	return root
}

// RotateOnce copy-truncates every live server log above MaxSize.
func (r *Rotator) RotateOnce() {
	for _, id := range r.live() {
		path := filepath.Join(ServerDir(r.cfg.Dir, id), id+".log")
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
	dir := ServerDir(r.cfg.Dir, id)
	for _, name := range []string{id + ".log.1", id + ".log"} {
		path := filepath.Join(dir, name)
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
// Since #994 the tree is two levels deep ({project}/{env}/{id}.log) for logs
// written by a labelling agent and flat for everything older, so the sweep
// walks instead of reading one directory — otherwise retention would silently
// stop reclaiming disk the moment the pair started riding in the path.
func (r *Rotator) SweepOnce() {
	live := map[string]bool{}
	for _, id := range r.live() {
		live[id] = true
	}
	cutoff := time.Now().Add(-r.cfg.Retention)
	staleCutoff := time.Now().Add(-2 * r.cfg.SweepEvery)

	err := filepath.WalkDir(r.cfg.Dir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable entry: skip, keep sweeping
		}
		name := e.Name()
		if e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil //nolint:nilerr
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
				return nil
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
		return nil
	})
	if err != nil {
		r.cfg.Logf("[logrot] sweep: %v", err)
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
