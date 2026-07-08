// Package upgrade implements the agent self-upgrade download path
// (docs/specs/agent.md §7): download → verify sha256 → atomic rename over the
// running binary. The restart itself is owned by the caller (systemd brings
// the new binary up after a clean exit).
package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// downloadTimeout bounds the whole fetch (release binaries are tens of MB).
const downloadTimeout = 5 * time.Minute

// Apply downloads rawURL (http, https or file scheme), verifies its sha256
// against wantHex and atomically replaces destPath. On any error destPath is
// left untouched.
func Apply(ctx context.Context, rawURL, wantHex, destPath string) error {
	want, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(wantHex)))
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("sha256 must be %d hex chars", sha256.Size*2)
	}

	src, err := open(ctx, rawURL)
	if err != nil {
		return err
	}
	defer src.Close()

	// The temp file lives next to the binary: rename(2) is atomic only
	// within one filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(destPath), filepath.Base(destPath)+".new-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), src); err != nil {
		tmp.Close()
		return fmt.Errorf("download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	got := h.Sum(nil)
	if !strings.EqualFold(hex.EncodeToString(got), hex.EncodeToString(want)) {
		return fmt.Errorf("sha256 mismatch: got %x", got)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}
	return nil
}

func open(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}
	switch u.Scheme {
	case "file":
		// file://host is meaningless here; only the path is honored.
		return os.Open(u.Path)
	case "http", "https":
		reqCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
		if err != nil {
			cancel()
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			cancel()
			return nil, fmt.Errorf("download: HTTP %d", resp.StatusCode)
		}
		return &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}, nil
	default:
		return nil, fmt.Errorf("unsupported url scheme %q (http, https, file)", u.Scheme)
	}
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
