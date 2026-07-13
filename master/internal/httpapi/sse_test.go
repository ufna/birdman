package httpapi_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// apiServer spins the REST API without a running matchmaker loop.
func apiServer(t *testing.T, st *store.Store) *httptest.Server {
	ts, _ := apiServerRec(t, st)
	return ts
}

// apiServerRec is apiServer + the recorder behind the deploy manager.
func apiServerRec(t *testing.T, st *store.Store) (*httptest.Server, *testdb.CommandRecorder) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	rec := &testdb.CommandRecorder{}
	dep := deploy.New(deploy.Options{Store: st, Sender: rec, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(ts.Close)
	return ts, rec
}

func scopedKey(t *testing.T, st *store.Store, name string, scopes ...string) string {
	t.Helper()
	_, key, err := st.CreateAPIKey(t.Context(), store.CreateAPIKeyParams{Name: name, Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// sseEvent is one parsed SSE frame.
type sseEvent struct {
	id   int64
	kind string
	data string
}

// sseClient reads an event stream line by line off the test goroutine.
type sseClient struct {
	resp  *http.Response
	lines chan string
}

func openSSE(t *testing.T, url, key string) *sseClient {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req) // no timeout: endless stream
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("stream: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("stream content-type: %s", ct)
	}
	c := &sseClient{resp: resp, lines: make(chan string, 256)}
	go func() {
		defer close(c.lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
	}()
	t.Cleanup(c.close)
	return c
}

func (c *sseClient) close() { c.resp.Body.Close() }

// next returns the next complete event frame (comments skipped).
func (c *sseClient) next(t *testing.T, timeout time.Duration) sseEvent {
	t.Helper()
	deadline := time.After(timeout)
	var ev sseEvent
	seen := false
	for {
		select {
		case <-deadline:
			t.Fatalf("no SSE event within %v", timeout)
		case line, ok := <-c.lines:
			if !ok {
				t.Fatal("SSE stream closed unexpectedly")
			}
			switch {
			case strings.HasPrefix(line, ":"), strings.HasPrefix(line, "retry:"):
				// comment / reconnect hint
			case strings.HasPrefix(line, "id: "):
				ev.id, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
				seen = true
			case strings.HasPrefix(line, "event: "):
				ev.kind = strings.TrimPrefix(line, "event: ")
				seen = true
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
				seen = true
			case line == "" && seen:
				return ev
			}
		}
	}
}

// TestEventsStream: auth, live delivery of new events, cursor replay via
// ?after_id (docs/specs/master.md §6 SSE).
func TestEventsStream(t *testing.T) {
	st := testdb.New(t)
	ts := apiServer(t, st)
	ctx := t.Context()
	roKey := scopedKey(t, st, "ro", httpapi.ScopeReadonly)

	// Auth matrix: anon and non-readonly scopes are rejected.
	if code, _ := (&client{t: t, base: ts.URL}).do("GET", "/v1/events/stream", nil); code != 401 {
		t.Fatalf("anon stream: want 401, got %d", code)
	}
	mmc := &client{t: t, base: ts.URL, key: scopedKey(t, st, "mm", httpapi.ScopeMatchmaking)}
	if code, _ := mmc.do("GET", "/v1/events/stream", nil); code != 403 {
		t.Fatalf("matchmaking-scope stream: want 403, got %d", code)
	}
	roc := &client{t: t, base: ts.URL, key: roKey}
	if code, _ := roc.do("GET", "/v1/events/stream?after_id=nope", nil); code != 400 {
		t.Fatal("bad after_id: want 400")
	}

	// Live delivery: events inserted after connect arrive with kind and data.
	c := openSSE(t, ts.URL+"/v1/events/stream", roKey)
	if err := st.InsertEvent(ctx, store.EventNodeQuarantine, store.EventRef{},
		map[string]any{"hostname": "n1"}); err != nil {
		t.Fatal(err)
	}
	ev := c.next(t, 5*time.Second)
	if ev.kind != store.EventNodeQuarantine {
		t.Fatalf("kind: want %s, got %+v", store.EventNodeQuarantine, ev)
	}
	var payload store.Event
	if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
		t.Fatalf("data is not json: %q: %v", ev.data, err)
	}
	if payload.ID != ev.id || fmt.Sprint(payload.Payload["hostname"]) != "n1" {
		t.Fatalf("payload mismatch: %+v vs id %d", payload, ev.id)
	}

	// Ordering: two more events arrive in insertion order.
	if err := st.InsertEvent(ctx, store.EventNodeRecovered, store.EventRef{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(ctx, store.EventCrashLoop, store.EventRef{}, nil); err != nil {
		t.Fatal(err)
	}
	ev2 := c.next(t, 5*time.Second)
	ev3 := c.next(t, 5*time.Second)
	if ev2.kind != store.EventNodeRecovered || ev3.kind != store.EventCrashLoop || ev3.id <= ev2.id {
		t.Fatalf("order: got %+v then %+v", ev2, ev3)
	}
	c.close()

	// Cursor replay: reconnect after the first event replays the rest.
	c2 := openSSE(t, fmt.Sprintf("%s/v1/events/stream?after_id=%d", ts.URL, ev.id), roKey)
	r2 := c2.next(t, 5*time.Second)
	r3 := c2.next(t, 5*time.Second)
	if r2.kind != store.EventNodeRecovered || r3.kind != store.EventCrashLoop {
		t.Fatalf("replay: got %+v then %+v", r2, r3)
	}
}
