package tui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Port of test/tui-events.test.ts (SSE /events subscription) plus the
// fetchSnapshot payload-shape and token-handling contracts from
// test/tui.test.ts — against hermetic httptest servers.

func fakeDaemon(t *testing.T) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var mu sync.Mutex
	var reqs []*http.Request
	token := "test-token"
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"error":"unauthorized"}`)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/status"):
			_, _ = fmt.Fprint(w, `{"now":"2026-09-07T00:00:00Z","uptime_s":5,"runs":{"active":0,"failed_recent":0},`+
				`"queue":{"pending":0,"claimed":0,"done":0},"circuit":"closed",`+
				`"herdr":{"enabled":true,"session":"devagent"},"spawn":{"visibility":"visible"},`+
				`"capabilities":["approve","dispatch","attach","kill-via-answer"]}`)
		case strings.HasPrefix(r.URL.Path, "/agents"):
			_, _ = fmt.Fprint(w, `{"panes":[],"queued":[]}`)
		case strings.HasPrefix(r.URL.Path, "/history"):
			_, _ = fmt.Fprint(w, `{"records":[{"ts":"2026-09-07T00:00:00Z","kind":"audit","taskId":"TASK-abc","attempt":1,"verdict":"pass"}]}`)
		case strings.HasPrefix(r.URL.Path, "/sessions"):
			_, _ = fmt.Fprint(w, `{"panes":[{"taskId":"TASK-abc","role":"worker","worker":"omp","paneId":"w1:p1","workspaceId":"w1","label":"TASK-abc-a1","cwd":"/tmp/w1","agentStatus":"working","state":"running","startedAt":"2026-09-07T00:00:00Z"}]}`)
		case strings.HasPrefix(r.URL.Path, "/approve"):
			_, _ = fmt.Fprint(w, `{"ok":true,"note":"killed"}`)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &reqs
}
func TestFetchSnapshotEnvelopeShapes(t *testing.T) {
	// Regression (2026-09-05 sessions crash): the envelopes must normalize —
	// sessions unwrapped from {panes:[...]}, history from {records:[...]}.
	srv, _ := fakeDaemon(t)
	snap := FetchSnapshot(TuiOptions{URL: srv.URL, Token: "test-token"})
	if !snap.Reachable {
		t.Fatal("snapshot must be reachable")
	}
	if snap.Sessions == nil || len(snap.Sessions) != 1 || snap.Sessions[0].PaneID != "w1:p1" {
		t.Fatalf("sessions = %+v", snap.Sessions)
	}
	if snap.Agents == nil || snap.Agents.Panes == nil || snap.Agents.Queued == nil {
		t.Fatalf("agents fields must be arrays: %+v", snap.Agents)
	}
	if len(snap.History) != 1 || snap.History[0]["taskId"] != "TASK-abc" {
		t.Fatalf("history = %+v", snap.History)
	}
	if snap.Status == nil || snap.Status.Herdr == nil || snap.Status.Herdr.Session != "devagent" {
		t.Fatalf("status = %+v", snap.Status)
	}
	out := plain(RenderDashboard(snap, RenderOptions{}))
	for _, want := range []string{"DevAgent", "0p/0c/0d", "herdr:devagent", "no workers, queue empty",
		"[1] workers [2] sessions [3] log", "k kill", "q] quit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("one-shot frame missing %q\n%s", want, out)
		}
	}
}

func TestFetchSnapshotUnreachable(t *testing.T) {
	// Port 1 on localhost is closed in every sandbox: the dead-port degrade.
	snap := FetchSnapshot(TuiOptions{URL: "http://127.0.0.1:1"})
	if snap.Reachable {
		t.Fatal("dead port must be unreachable")
	}
	out := RenderDashboard(snap, RenderOptions{})
	if !strings.Contains(out, "DAEMON UNREACHABLE") {
		t.Fatal("must degrade to UNREACHABLE instead of throwing")
	}
}

func TestFetchSnapshotAuthRejected(t *testing.T) {
	srv, _ := fakeDaemon(t)
	snap := FetchSnapshot(TuiOptions{URL: srv.URL, Token: "wrong-token"})
	if !snap.AuthFailed {
		t.Fatal("401 must set authFailed")
	}
	out := RenderDashboard(snap, RenderOptions{})
	if !strings.Contains(out, "DAEMON AUTH REJECTED") {
		t.Fatal("auth failure must render the rejected header")
	}
}

func TestPostJSONNotes(t *testing.T) {
	srv, _ := fakeDaemon(t)
	res := PostJSON(TuiOptions{URL: srv.URL, Token: "test-token"}, "/approve",
		map[string]any{"repoPath": "/tmp/repo", "taskId": "TASK-abc", "answer": "__kill__"})
	if !res.OK || res.Note != "killed" {
		t.Fatalf("approve result = %+v", res)
	}
	res = PostJSON(TuiOptions{URL: srv.URL, Token: "wrong-token"}, "/approve", nil)
	if res.OK || res.Status != 401 {
		t.Fatalf("401 result = %+v", res)
	}
	res = PostJSON(TuiOptions{URL: "http://127.0.0.1:1"}, "/approve", nil)
	if res.Status != 0 || res.OK || res.Note != "daemon unreachable" {
		t.Fatalf("dead port note = %+v", res)
	}
}

func TestResolveTokenChain(t *testing.T) {
	t.Setenv("DEVAGENT_DAEMON_TOKEN", "")
	home := t.TempDir()
	t.Setenv("DEVAGENT_HOME", home)
	if got := ResolveToken(TuiOptions{Token: "opt"}); got != "opt" {
		t.Fatal("opts must win")
	}
	t.Setenv("DEVAGENT_DAEMON_TOKEN", "env-token")
	if got := ResolveToken(TuiOptions{}); got != "env-token" {
		t.Fatal("env must come second")
	}
	t.Setenv("DEVAGENT_DAEMON_TOKEN", "")
	if err := os.WriteFile(filepath.Join(home, "daemon-token"), []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveToken(TuiOptions{}); got != "file-token" {
		t.Fatalf("file fallback = %q", got)
	}
	// Traversal-shaped DEVAGENT_HOME is refused outright.
	t.Setenv("DEVAGENT_HOME", filepath.Join(home, "..", "escape"))
	if got := ResolveToken(TuiOptions{}); got != "" {
		t.Fatalf("traversal home must resolve no token: %q", got)
	}
}

func sseServer(t *testing.T, events []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		lastID := -1
		if v := r.Header.Get("Last-Event-ID"); v != "" {
			_, _ = fmt.Sscanf(v, "%d", &lastID)
		}
		for i, ev := range events {
			if i <= lastID {
				continue // replay skips what we already have
			}
			_, _ = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", i, ev)
			flusher.Flush()
		}
		<-r.Context().Done() // hold the stream open like a live tail
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func waitFor(t *testing.T, cond func() bool, ms int) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSubscribeEventsReplay(t *testing.T) {
	// Bounded: the file never grows, so exactly the replayed tail arrives.
	srv := sseServer(t, []string{`{"message":"seed-one"}`, `{"message":"seed-two"}`})
	var mu sync.Mutex
	var got []string
	var states []EventsState
	appendEvent := func(_ int, data string) { mu.Lock(); got = append(got, data); mu.Unlock() }
	appendState := func(st EventsState) { mu.Lock(); states = append(states, st); mu.Unlock() }
	events := func() []string { mu.Lock(); defer mu.Unlock(); return got }
	sub := SubscribeEvents(TuiOptions{URL: srv.URL, Token: "tok"}, appendEvent, appendState)
	defer sub.Stop()
	waitFor(t, func() bool { return len(events()) >= 2 }, 4000)
	gotEv := events()
	if len(gotEv) != 2 {
		t.Fatalf("events = %v, want exactly the replayed tail", gotEv)
	}
	if !strings.Contains(gotEv[0], "seed-one") || !strings.Contains(gotEv[1], "seed-two") {
		t.Fatalf("events = %v", gotEv)
	}
	mu.Lock()
	defer mu.Unlock()
	live := false
	for _, st := range states {
		if st == StateLive {
			live = true
		}
	}
	if !live {
		t.Fatalf("states = %v, want live", states)
	}
}
func TestSubscribeEventsResumeFromLastEventID(t *testing.T) {
	srv := sseServer(t, []string{`{"message":"seed-one"}`, `{"message":"seed-two"}`})
	var mu sync.Mutex
	var got []string
	sub := SubscribeEvents(TuiOptions{URL: srv.URL, Token: "tok"},
		func(_ int, data string) { mu.Lock(); got = append(got, data); mu.Unlock() },
		nil,
		0, // id 0 skipped
	)
	defer sub.Stop()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], "seed-two") {
		t.Fatalf("resume events = %v, want only seed-two", got)
	}
}

func TestSubscribeEventsStopWithoutThrow(t *testing.T) {
	srv := sseServer(t, []string{`{"message":"seed-one"}`})
	sub := SubscribeEvents(TuiOptions{URL: srv.URL, Token: "tok"}, nil, nil)
	sub.Stop()
	sub.Stop() // idempotent
	if sub.LastEventID() != -1 {
		t.Fatalf("lastEventId = %d, want -1", sub.LastEventID())
	}
}

func TestDecodeKeysLoopSeam(t *testing.T) {
	// The Loop seam consumes DecodeKeys output; smoke the contract shape.
	res := DecodeKeys("jk\x1b[A", false)
	if len(res.Keys) != 3 || res.Keys[2].Kind != KeyUp {
		t.Fatalf("loop chunk = %+v", res)
	}
}
