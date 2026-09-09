package tui

// Contract tests for FR-TUI-P: attach-resume hardening (P-01), poisoned
// payload shapes (P-02), failed_recent never paints FAILED (P-03),
// SIGWINCH resize (P-04), hero card (P-05), narrow stacking (P-08),
// incremental redraw (P-10) and the help quit row at 24x80 (P-12).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- P-02: poisoned payload shapes ---------------------------------------

// poisonDaemon serves the daemon endpoints with WRONG JSON shapes: arrays
// where the code expects objects and objects where it expects arrays. The
// dashboard must normalize/degrade to empty — never panic, never throw.
func poisonDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/status"):
			// status as an ARRAY of strings instead of an object.
			_, _ = w.Write([]byte(`["status","should","be","an","object"]`))
		case strings.HasPrefix(r.URL.Path, "/agents"):
			// panes/queued as objects-where-arrays (the 2026-09-05 crash).
			_, _ = w.Write([]byte(`{"panes":{"0":{"taskId":"TASK-poison"}},"queued":{"0":{"id":"TASK-q"}}}`))
		case strings.HasPrefix(r.URL.Path, "/history"):
			// records as an object-where-array too.
			_, _ = w.Write([]byte(`{"records":{"0":{"taskId":"TASK-poison"}}}`))
		case strings.HasPrefix(r.URL.Path, "/sessions"):
			// panes as a bare object-where-array.
			_, _ = w.Write([]byte(`{"panes":{"0":{"paneId":"p:poison"}}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchSnapshotPoisonedShapes(t *testing.T) {
	srv := poisonDaemon(t)
	snap := FetchSnapshot(TuiOptions{URL: srv.URL, Token: "tok"})
	// Status object missing → nil payload, still "reachable" HTTP-wise.
	if snap.Status != nil {
		t.Fatalf("array status must decode to nil payload: %+v", snap.Status)
	}
	// Object-where-array must degrade to empty, not panic.
	if snap.Agents == nil {
		t.Fatal("agents payload must still normalize to a struct")
	}
	if len(snap.Agents.Panes) != 0 || len(snap.Agents.Queued) != 0 {
		t.Fatalf("object-where-array panes/queued must degrade to empty: %+v", snap.Agents)
	}
	if len(snap.History) != 0 {
		t.Fatalf("object-where-array records must degrade to empty: %+v", snap.History)
	}
	if len(snap.Sessions) != 0 {
		t.Fatalf("object-where-array sessions must degrade to empty: %+v", snap.Sessions)
	}
	// The whole point: rendering the poisoned snapshot must not panic.
	out := plain(RenderDashboard(snap, RenderOptions{}))
	if !strings.Contains(out, "DevAgent") {
		t.Fatal("dashboard still renders its frame on poisoned payloads")
	}
}

func TestNormalizeHelpersPoisoned(t *testing.T) {
	// Direct unit probes of the normalize seams with poisoned shapes.
	if got := NormalizeAgents(map[string]any{
		"panes":  map[string]any{"0": map[string]any{"taskId": "x"}},
		"queued": "not-an-array",
	}); got == nil || len(got.Panes) != 0 || len(got.Queued) != 0 {
		t.Fatalf("NormalizeAgents poisoned = %+v, want empty arrays", got)
	}
	if got := NormalizeAgents("not-an-object"); got != nil {
		t.Fatalf("NormalizeAgents non-object = %+v, want nil", got)
	}
}

// --- P-03: failed_recent never paints FAILED ------------------------------

func TestFailedRecentNeverFailsAggregate(t *testing.T) {
	snap := testSnapshot()
	// Historical failures present, every live signal idle, breaker closed.
	snap.Status.Runs.Active = fptr(0)
	snap.Status.Queue.Claimed = fptr(0)
	snap.Status.Queue.Pending = fptr(0)
	snap.Agents.Panes[0].State = "idle"
	out := plain(RenderDashboard(snap, RenderOptions{}))
	if strings.Contains(out, "FAILED") {
		t.Fatalf("failed_recent>0 with all panes idle painted FAILED:\n%s", out)
	}
	if !strings.Contains(out, "IDLE") {
		t.Fatal("all-idle live state must aggregate to IDLE")
	}
}

func TestAggregateStatusIgnoresFailedRecent(t *testing.T) {
	status := &StatusPayload{
		Runs: &RunsPayload{Active: fptr(0), FailedRecent: fptr(9)},
	}
	if got := AggregateStatus(status, nil); got != "IDLE" {
		t.Fatalf("AggregateStatus = %q, want IDLE (failed_recent is historical)", got)
	}
}

// --- P-05: hero card -------------------------------------------------------

func TestHeroShowsRunningPane(t *testing.T) {
	snap := testSnapshot()
	out := plain(RenderDashboard(snap, RenderOptions{Selection: 0}))
	for _, want := range []string{"TASK-abc", "working"} {
		if !strings.Contains(out, want) {
			t.Fatalf("hero missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "%") {
		t.Fatal("hero must not fabricate a progress percent")
	}
}

func TestHeroIdleShowsNextAction(t *testing.T) {
	snap := testSnapshot()
	snap.Agents.Panes[0].State = "idle"
	out := plain(RenderDashboard(snap, RenderOptions{}))
	if !strings.Contains(out, "next:") {
		t.Fatalf("idle hero must show a next-action cue:\n%s", out)
	}
}

// --- P-08: narrow terminal stacking ----------------------------------------

func TestNarrowTerminalStacksCards(t *testing.T) {
	snap := testSnapshot()
	snap.Agents.Panes = append(snap.Agents.Panes, TuiPane{
		TaskID: "TASK-second", PaneID: "w1:p2", State: "idle", Cwd: "/tmp/second",
	})
	out := RenderDashboard(snap, RenderOptions{Width: 78})
	lines := strings.Split(out, "\n")
	for _, ln := range lines {
		if VisibleLen(ln) > 78 {
			t.Fatalf("78-col frame overflows: %d cells (%q)", VisibleLen(ln), ln)
		}
	}
	// Full-width stacking: both cards appear with their own box top (no
	// 2-up side-by-side crushing).
	if got := strings.Count(plain(out), "╭─"); got != 3 { // two worker cards + queued card
		t.Fatalf("narrow render must stack every card in its own box, box tops = %d", got)
	}
}

func TestWideTerminalKeepsTwoUp(t *testing.T) {
	snap := testSnapshot()
	out := plain(RenderDashboard(snap, RenderOptions{Width: 120}))
	if strings.Count(out, "╭─") != 2 { // worker + queued share rows 2-up
		t.Fatalf("wide render should pair cards 2-up, box tops = %d", strings.Count(out, "╭─"))
	}
}

// --- P-10: incremental redraw guarantees ------------------------------------

func TestConsecutiveIdenticalFramesEmitNoWrites(t *testing.T) {
	// Two identical renders through the differ: zero row writes, i.e. the
	// sequence carries no erase-to-EOL or text payload.
	first := RenderLines(testSnapshot(), RenderOptions{Rows: 24, Width: 80})
	seq := RenderFrame(first, first, 80)
	if strings.Contains(seq, "\x1b[K") || strings.Contains(seq, "\x1b[2J") {
		t.Fatalf("identical frames must emit zero row writes, got %q", seq)
	}
}

func TestWidthChangeFullClearsExactlyOnce(t *testing.T) {
	snap := testSnapshot()
	before := RenderLines(snap, RenderOptions{Rows: 24, Width: 80})
	after := RenderLines(snap, RenderOptions{Rows: 24, Width: 100})
	seq := RenderFrame(before, after, 100)
	if strings.Count(seq, "\x1b[2J") != 0 {
		t.Fatal("RenderFrame itself never emits 2J; the loop owns the resize clear")
	}
}

func TestSingleRowChangeRewritesOnlyThatRow(t *testing.T) {
	snap := testSnapshot()
	before := RenderLines(snap, RenderOptions{Rows: 24, Width: 80})
	after := RenderLines(snap, RenderOptions{Rows: 24, Width: 80, Note: "operator note"})
	seq := RenderFrame(before, after, 80)
	writes := strings.Count(seq, "\x1b[K")
	// The note renders in the footer row — a note change must rewrite
	// exactly that one row; every other row is skipped by the differ.
	if writes != 1 {
		t.Fatalf("note change should rewrite exactly 1 row, got %d", writes)
	}
}

// --- P-12: help quit row at 24x80 --------------------------------------------

func TestHelpQuitRowAt24x80(t *testing.T) {
	snap := testSnapshot()
	out := plain(RenderDashboard(snap, RenderOptions{Rows: 24, Width: 80, ShowHelp: true}))
	if !strings.Contains(out, "q or Ctrl+C") {
		t.Fatalf("quit row must survive a 24x80 help overlay:\n%s", out)
	}
	if strings.Count(out, "\n") > 24 {
		t.Fatalf("help frame exceeds 24 rows")
	}
}
func TestHelpQuitRowAtVeryShortTerminal(t *testing.T) {
	snap := testSnapshot()
	out := plain(RenderDashboard(snap, RenderOptions{Rows: 10, Width: 80, ShowHelp: true}))
	if !strings.Contains(out, "q or Ctrl+C") {
		t.Fatalf("quit row must survive even a 10-row help overlay:\n%s", out)
	}
}

func TestFooterHintTiers(t *testing.T) {
	if h := footerHint(ViewWorkers, 116); !strings.Contains(h, "a attach") {
		t.Fatal("full tier (>=116) must include attach")
	}
	if h := footerHint(ViewWorkers, 100); strings.Contains(h, "r refresh") {
		t.Fatalf("100-col tier must drop the trailing r refresh: %q", h)
	}
	if h := footerHint(ViewWorkers, 100); !strings.Contains(h, "[q] quit") {
		t.Fatal("100-col tier must keep [q] quit (the P-12 regression)")
	}
	if h := footerHint(ViewWorkers, 50); !strings.Contains(h, "[?] help [q] quit") {
		t.Fatalf("narrow tier (50) must fold to help/quit: %q", h)
	}
	if h := footerHint(ViewLog, 100); !strings.Contains(h, "f follow") {
		t.Fatalf("log view tier must keep f follow: %q", h)
	}
}

// --- P-01/P-04: run-level loop tests -----------------------------------------

// winchEnv is a bufEnv whose Sigwinch() returns a live channel the test can
// fire; it can also panic inside SuspendAttach (P-01 env-crash path).
type winchEnv struct {
	*bufEnv
	mu      sync.Mutex
	winch   chan os.Signal
	panicIt bool
}

func newWinchEnv() *winchEnv {
	return &winchEnv{bufEnv: newBufEnv(), winch: make(chan os.Signal, 4)}
}

func (e *winchEnv) Sigwinch() <-chan os.Signal { return e.winch }

func (e *winchEnv) SuspendAttach(paneID, taskID, repoPath string) int {
	e.mu.Lock()
	panicIt := e.panicIt
	e.mu.Unlock()
	if panicIt {
		panic("env attach exploded")
	}
	return e.bufEnv.SuspendAttach(paneID, taskID, repoPath)
}

func TestLoopRunAttachEnvPanicResumesDashboard(t *testing.T) {
	env := newWinchEnv()
	env.panicIt = true
	env.cols = 140 // the note shares one footer line with the hint
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	tr.snap = &Snapshot{
		Status:    &StatusPayload{},
		Agents:    &AgentPayload{Panes: []TuiPane{{TaskID: "T1", PaneID: "p1"}}},
		Reachable: true,
	}
	l := NewLoop(TuiOptions{RepoPath: "/repo"}, tr, env, "attach").(*loop)
	done := make(chan error, 1)
	go func() { done <- l.Run() }()

	runWaitFor(t, func() bool { return l.Snapshot().Agents != nil }, "first poll never landed")
	env.in.write("a")
	// The dashboard must survive: still running, screen re-entered, crash
	// surfaced in the note, and a fresh poll fired.
	runWaitFor(t, func() bool {
		return strings.Contains(env.buf.String(), "attach crashed")
	}, "attach crash note never rendered")
	runWaitFor(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return !l.suspended
	}, "loop stuck suspended after attach crash")
	env.in.write("q")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after attach crash = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after attach crash")
	}
	if !env.screenEntered() {
		t.Fatal("screen must be re-entered after the attach crash")
	}
}

func TestLoopRunSigwinchRepaintsPromptly(t *testing.T) {
	env := newWinchEnv()
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	l := NewLoop(TuiOptions{}, tr, env, "attach").(*loop)
	done := make(chan error, 1)
	go func() { done <- l.Run() }()

	runWaitFor(t, func() bool {
		return strings.Contains(env.buf.String(), "[1] workers")
	}, "first frame never rendered")

	// Resize: narrower geometry, then fire SIGWINCH. drawLocked re-probes
	// Size and full-clears (the sanctioned reflow clear).
	env.bufEnv.cols = 60
	env.winch <- os.Interrupt // any signal value wakes the repaint
	runWaitFor(t, func() bool {
		return strings.Contains(env.buf.String(), "\x1b[2J")
	}, "SIGWINCH never triggered the reflow repaint")

	env.in.write("q")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after SIGWINCH")
	}
}
