package logrot

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Streaming of a server log for TailLogs (docs/specs/agent.md §5): the
// archived part first ({id}.log.1[.gz]), then the current file
// ({id}.log[.gz]); follow keeps polling the active {id}.log for appended
// data (rotation-aware) until ctx is done or the log is finalized (server
// stopped → files gzipped → eof).

const (
	chunkSize    = 32 * 1024
	followPoll   = 300 * time.Millisecond
	maxTailLines = 10000
)

// ErrNoLogs is returned when no log files exist for the server.
var ErrNoLogs = errors.New("no logs for server")

// Stream emits the server's log via emit(chunk). tailLines > 0 → only the
// last N lines of the existing content are emitted (capped at maxTailLines);
// follow → after the existing content, keep following {id}.log. emit errors
// abort the stream and are returned as-is.
func Stream(ctx context.Context, dir, id string, tailLines int, follow bool, emit func([]byte) error) error {
	// dir is the ROOT of the server log tree; since #994 a labelled dedik
	// writes into {root}/{project}/{env}/, so resolve the actual directory
	// from the filesystem (see ServerDir). Live-tail of a reaped server keeps
	// working: the resolution looks at files, not at the in-memory map.
	root := dir
	dir = ServerDir(root, id)
	activePath := filepath.Join(dir, id+".log")

	var sources []string
	for _, cand := range []string{id + ".log.1.gz", id + ".log.1", id + ".log.gz", id + ".log"} {
		p := filepath.Join(dir, cand)
		if _, err := os.Stat(p); err == nil {
			sources = append(sources, p)
		}
	}
	if len(sources) == 0 {
		if !follow {
			return ErrNoLogs
		}
		// follow of a server that has not logged yet: wait for the file.
	}

	// offset in the active plain log after the backlog pass — the follow
	// cursor. -1 means the active plain log was not among the sources.
	activeOffset := int64(-1)

	if tailLines > 0 {
		if tailLines > maxTailLines {
			tailLines = maxTailLines
		}
		ring := make([][]byte, 0, tailLines)
		for _, src := range sources {
			err := readLines(ctx, src, func(line []byte) {
				cp := append([]byte(nil), line...)
				if len(ring) == tailLines {
					ring = append(ring[1:], cp)
				} else {
					ring = append(ring, cp)
				}
			})
			if err != nil {
				return err
			}
		}
		var buf bytes.Buffer
		for _, line := range ring {
			buf.Write(line)
			buf.WriteByte('\n')
			if buf.Len() >= chunkSize {
				if err := emit(copyBytes(buf.Bytes())); err != nil {
					return err
				}
				buf.Reset()
			}
		}
		if buf.Len() > 0 {
			if err := emit(copyBytes(buf.Bytes())); err != nil {
				return err
			}
		}
		if st, err := os.Stat(activePath); err == nil {
			activeOffset = st.Size()
		}
	} else {
		for _, src := range sources {
			n, err := streamFile(ctx, src, emit)
			if err != nil {
				return err
			}
			if src == activePath {
				activeOffset = n
			}
		}
	}

	if !follow {
		return nil
	}
	return followFile(ctx, root, id, activePath, activeOffset, emit)
}

// followFile polls path for appended data from offset (-1 → from current
// end once the file appears). Truncation (copy-truncate rotation) resets the
// cursor to 0. The follow ends cleanly when the file is finalized (gone,
// with a .gz left behind — the server stopped).
//
// While the file has never been seen (offset < 0) the path is re-resolved on
// every miss: following a server that has not logged yet starts before its
// directory exists, and since #994 that directory depends on the dedik's
// (project, env) — resolving once would follow the flat fallback forever.
func followFile(ctx context.Context, root, id, path string, offset int64, emit func([]byte) error) error {
	buf := make([]byte, chunkSize)
	t := time.NewTicker(followPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		st, err := os.Stat(path)
		if err != nil {
			if _, gzErr := os.Stat(path + ".gz"); gzErr == nil && offset >= 0 {
				return nil // finalized: the server stopped, backlog was emitted
			}
			if offset < 0 {
				path = filepath.Join(ServerDir(root, id), id+".log")
			}
			continue // not created yet (or transient) — keep waiting
		}
		if offset < 0 || st.Size() < offset {
			// First sight of the file → its beginning; shrunk → rotated
			// under us (copy-truncate) → new content starts at 0.
			offset = 0
		}
		if st.Size() == offset {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return err
		}
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				offset += int64(n)
				if err := emit(copyBytes(buf[:n])); err != nil {
					f.Close()
					return err
				}
			}
			if rerr != nil {
				break
			}
		}
		f.Close()
	}
}

// streamFile emits the whole file (gunzipping *.gz) in chunks and returns the
// plain-file size consumed (0 for gzip sources — they are never followed).
func streamFile(ctx context.Context, path string, emit func([]byte) error) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var rd io.Reader = f
	gzipped := filepath.Ext(path) == ".gz"
	if gzipped {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return 0, err
		}
		defer zr.Close()
		rd = zr
	}
	var n int64
	buf := make([]byte, chunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		m, rerr := rd.Read(buf)
		if m > 0 {
			if !gzipped {
				n += int64(m)
			}
			if err := emit(copyBytes(buf[:m])); err != nil {
				return n, err
			}
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, rerr
		}
	}
}

// readLines feeds every line of the (possibly gzipped) file to fn.
func readLines(ctx context.Context, path string, fn func(line []byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var rd io.Reader = f
	if filepath.Ext(path) == ".gz" {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer zr.Close()
		rd = zr
	}
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		fn(sc.Bytes())
	}
	return sc.Err()
}

func copyBytes(b []byte) []byte { return append([]byte(nil), b...) }
