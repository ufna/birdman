package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func dest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "birdman-agent")
	if err := os.WriteFile(path, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestApplyHTTP(t *testing.T) {
	payload := []byte("new-binary-contents")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer ts.Close()

	path := dest(t)
	if err := Apply(context.Background(), ts.URL+"/agent", sha(payload), path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("binary = %q, %v", got, err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", st.Mode())
	}
	// No leftover temp files.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestApplyFileURL(t *testing.T) {
	payload := []byte("file-scheme-binary")
	srcPath := filepath.Join(t.TempDir(), "agent-new")
	if err := os.WriteFile(srcPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	path := dest(t)
	if err := Apply(context.Background(), "file://"+srcPath, strings.ToUpper(sha(payload)), path); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(payload) {
		t.Fatalf("binary = %q", got)
	}
}

func TestApplyShaMismatchKeepsBinary(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("evil"))
	}))
	defer ts.Close()

	path := dest(t)
	err := Apply(context.Background(), ts.URL, sha([]byte("expected")), path)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "old-binary" {
		t.Fatalf("binary must be untouched, got %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp file not cleaned up: %v", entries)
	}
}

func TestApplyBadInputs(t *testing.T) {
	path := dest(t)
	if err := Apply(context.Background(), "http://x", "not-hex", path); err == nil {
		t.Fatal("bad sha accepted")
	}
	if err := Apply(context.Background(), "ftp://x", sha([]byte("a")), path); err == nil ||
		!strings.Contains(err.Error(), "unsupported url scheme") {
		t.Fatalf("ftp err = %v", err)
	}
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	if err := Apply(context.Background(), ts.URL, sha([]byte("a")), path); err == nil ||
		!strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("404 err = %v", err)
	}
}
