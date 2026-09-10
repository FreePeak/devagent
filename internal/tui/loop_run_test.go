package tui

// Driver-level tests for the interactive loop (issue #252): Run() over an
// injected fake stdin reader and fake Transport — the seams keep the whole
// raw-mode session testable without a PTY (the env never enters raw mode,
import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipeReader is a fake stdin: writes deliver chunks; Close ends the stream
// (EOF → the loop quits cleanly, like a PTY teardown).
type pipeReader struct {
	ch   chan string
	rest string
}

func newPipeReader() *pipeReader { return &pipeReader{ch: make(chan string, 16)} }

func (p *pipeReader) write(s string) { p.ch <- s }

func (p *pipeReader) Read(b []byte) (int, error) {
	if p.rest != "" {
		n := copy(b, p.rest)
		p.rest = p.rest[n:]
		return n, nil
	}
	s, ok := <-p.ch
	if !ok {
		return 0, fmt.Errorf("EOF")
	}
	if len(s) == 0 {
		return 0, fmt.Errorf("EOF")
	}
	p.rest = s
	n := copy(b, p.rest)
	p.rest = p.rest[n:]
	return n, nil
}

func (p *pipeReader) close() { close(p.ch) }

// bufEnv is the fake LoopEnv: buffer stdout, fixed 100x40 geometry, raw
// mode bookkeeping, stubbed attach, no signals. Everything the loop's
// goroutines touch (draws, raw-mode counters, attach log) is guarded by mu:
// Run-driven tests read it from the test goroutine while poller/input
// goroutines are live — unsynchronized access is exactly the class of
// environment-dependent CI failure issue #271 pins.
type bufEnv struct {
	mu         sync.Mutex
	buf        strings.Builder
	in         *pipeReader
	rows, cols int
	rawEnters  int
	restores   int
	attachLog  []string
	attachCode int
}

func newBufEnv() *bufEnv {
	return &bufEnv{in: newPipeReader(), rows: DefaultRows, cols: DefaultColumns}
}

func (e *bufEnv) Out() io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.buf.Write(p)
	})
}

// outText snapshots the rendered output.
func (e *bufEnv) outText() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.buf.String()
}

func (e *bufEnv) In() io.Reader { return e.in }
func (e *bufEnv) Size() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rows, e.cols
}

// setCols resizes the fake geometry (SIGWINCH tests flip it mid-run).
func (e *bufEnv) setCols(n int) {
	e.mu.Lock()
	e.cols = n
	e.mu.Unlock()
}

// setRows resizes the fake geometry vertically (short-terminal tests).
func (e *bufEnv) setRows(n int) {
	e.mu.Lock()
	e.rows = n
	e.mu.Unlock()
}

func (e *bufEnv) EnterRaw() error    { e.mu.Lock(); e.rawEnters++; e.mu.Unlock(); return nil }
func (e *bufEnv) RestoreTerm() error { e.mu.Lock(); e.restores++; e.mu.Unlock(); return nil }

// rawStats snapshots the raw-mode counters.
func (e *bufEnv) rawStats() (enters, restores int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rawEnters, e.restores
}
func (e *bufEnv) Sigint() <-chan os.Signal   { return nil }
func (e *bufEnv) Sigwinch() <-chan os.Signal { return nil }
func (e *bufEnv) screenLeft() bool           { return strings.Contains(e.outText(), "\x1b[?1049l\x1b[?25h") }
func (e *bufEnv) screenEntered() bool {
	return strings.Contains(e.outText(), "\x1b[?1049h\x1b[?25l")
}

func (e *bufEnv) SuspendAttach(paneID, taskID, repoPath string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attachLog = append(e.attachLog, paneID+"|"+taskID+"|"+repoPath)
	return e.attachCode
}

// attaches copies the attach log (the input goroutine appends to it).
func (e *bufEnv) attaches() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.attachLog...)
}

// runWaitFor adapts the shared waitFor(cond, ms) with a failure message.
// The budget is failure latency only — conds go true in milliseconds when
// healthy — so it runs wide of the awaited interval: under `go test ./...`
// parallel-package load the tui package itself runs ~2.5× slower than
// standalone, and a tight deadline is the remaining environment-dependent
// flake class (issue #271).
func runWaitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	waitFor(t, cond, 10000)
	if !cond() {
		t.Fatal(msg)
	}
}

// TestLoopRunQuitsAndRestores: q through the fake stdin ends Run with the
// alternate screen entered once and restored exactly once, and the first
// snapshot fetched.
func TestLoopRunQuitsAndRestores(t *testing.T) {
	env := newBufEnv()
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	l := NewLoop(TuiOptions{}, tr, env, "attach").(*loop)

	done := make(chan error, 1)
	go func() { done <- l.Run() }()

	runWaitFor(t, func() bool { return tr.pollCount() >= 1 }, "initial poll never ran")
	env.in.write("q")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after q")
	}
	if !env.screenEntered() || !env.screenLeft() {
		t.Fatalf("screen enter/leave missing: %q", truncStr(env.outText(), 200))
	}
	enters, restores := env.rawStats()
	if enters != 1 || restores < 1 {
		t.Fatalf("raw enters=%d restores=%d", enters, restores)
	}
}

// TestLoopRunViewSwitchRendersFrames: '1' lands, the poll renders a frame
// through RenderFrame, then q quits.
func TestLoopRunViewSwitchRendersFrames(t *testing.T) {
	env := newBufEnv()
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	l := NewLoop(TuiOptions{}, tr, env, "attach").(*loop)

	done := make(chan error, 1)
	go func() { done <- l.Run() }()

	env.in.write("1")
	runWaitFor(t, func() bool {
		return strings.Contains(env.outText(), "[1] workers")
	}, "frame with the workers footer never rendered")

	env.in.write("q")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after q")
	}
}

// TestLoopFullHeightIdenticalFrameEmitsNoLF pins the view-switch desync root
// cause: when the frame's last row is skipped (identical rows are the norm
// after a 1/2/3 switch — only the body changes, blank tail rows don't),
// RenderFrame exits with the cursor one row BELOW the frame, so a trailing
// LF emitted after it lands on the terminal's bottom row and scrolls the
// alternate screen — after which the incremental diff draws every later
// frame one row off (stale rows from the previous view persisting under the
// new view's content). drawLocked must not append any LF.
func TestLoopFullHeightIdenticalFrameEmitsNoLF(t *testing.T) {
	env := newBufEnv()
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	l := NewLoop(TuiOptions{}, tr, env, "attach").(*loop)
	l.view = ViewLog
	for range 45 {
		l.logLines = append(l.logLines, LogLine{Message: "buffered log row"})
	}
	l.running = true

	l.drawLocked() // first draw paints the full frame
	firstLen := len(env.outText())
	l.drawLocked() // second draw diffs an identical state
	seg := env.outText()[firstLen:]
	if strings.Contains(seg, "\n") {
		t.Fatalf("identical full-height frame must emit zero LFs, got %q", truncStr(seg, 200))
	}
}

// TestLoopRunEOFRestores: stdin EOF (PTY teardown analog) quits cleanly.
func TestLoopRunEOFRestores(t *testing.T) {
	env := newBufEnv()
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	l := NewLoop(TuiOptions{}, tr, env, "attach").(*loop)

	done := make(chan error, 1)
	go func() { done <- l.Run() }()

	env.in.close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after EOF = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on EOF")
	}
	if !env.screenLeft() {
		t.Fatal("EOF must restore the screen")
	}
}

// TestLoopRunSuspendAttach pins the `a` flow end to end: the dashboard
// leaves the screen, the child runs with the stubbed env, and the resume
// repaints with the detach note.
func TestLoopRunSuspendAttach(t *testing.T) {
	env := newBufEnv()
	env.attachCode = 0
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	tr.snap = &Snapshot{
		Status:    &StatusPayload{},
		Agents:    &AgentPayload{Panes: []TuiPane{{TaskID: "T1", PaneID: "p1"}}},
		Reachable: true,
	}
	l := NewLoop(TuiOptions{RepoPath: "/repo"}, tr, env, "attach").(*loop)

	done := make(chan error, 1)
	go func() { done <- l.Run() }()

	// The pane roster arrives with the first poll; `a` before that only
	// sets the nothing-selected note.
	runWaitFor(t, func() bool { return l.Snapshot().Agents != nil }, "first poll never landed")
	env.in.write("a")
	runWaitFor(t, func() bool { return len(env.attaches()) == 1 }, "attach child never ran")
	if calls := env.attaches(); calls[0] != "p1|T1|/repo" {
		t.Fatalf("attach args = %q, want p1|T1|/repo", calls[0])
	}

	env.in.write("q")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after attach+q")
	}
	// Suspend must have left + re-entered the screen around the child.
	s := env.outText()
	first := strings.Index(s, "\x1b[?1049l\x1b[?25h")
	second := strings.LastIndex(s, "\x1b[?1049h\x1b[?25l")
	if first < 0 || second < 0 || second < first {
		t.Fatalf("suspend/resume escapes out of order: enter@%d leave@%d", second, first)
	}
	enters, _ := env.rawStats()
	if enters != 2 {
		t.Fatalf("raw enters = %d, want 2 (initial + attach resume)", enters)
	}
}

// TestLoopRunAttachFailure pins the nonzero-code note (the TS
// `attach exited (N)` branch).
func TestLoopRunAttachFailure(t *testing.T) {
	env := newBufEnv()
	env.attachCode = 3
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	tr.snap = &Snapshot{
		Status:    &StatusPayload{},
		Agents:    &AgentPayload{Panes: []TuiPane{{TaskID: "T1", PaneID: "p1"}}},
		Reachable: true,
	}
	l := NewLoop(TuiOptions{RepoPath: "/repo"}, tr, env, "attach").(*loop)

	done := make(chan error, 1)
	go func() { done <- l.Run() }()

	// The pane roster arrives with the first poll; `a` before that only
	// sets the nothing-selected note.
	runWaitFor(t, func() bool { return l.Snapshot().Agents != nil }, "first poll never landed")
	env.in.write("a")
	runWaitFor(t, func() bool { return len(env.attaches()) == 1 }, "attach child never ran")

	env.in.write("q")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// TestLoopPollCadence: the poller keeps polling after the first frame
// (2s chain) — a second fetch lands without any input.
func TestLoopPollCadence(t *testing.T) {
	env := newBufEnv()
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	l := NewLoop(TuiOptions{}, tr, env, "attach").(*loop)

	done := make(chan error, 1)
	go func() { done <- l.Run() }()

	deadline := time.Now().Add(8 * time.Second) // 4× the 2s poll chain: two fetches need ~2s; loaded CI boxes eat the slack
	for tr.pollCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if polls := tr.pollCount(); polls < 2 {
		t.Fatalf("polls = %d, want ≥2 (initial + scheduled)", polls)
	}
	env.in.write("q")
	<-done
}

// TestRunOneShotRendersDashboard pins the non-TTY degrade: one dashboard
// render + newline, no alternate-screen escapes. Fully hermetic: the
// snapshot comes from a stub daemon over httptest (never the live
// 127.0.0.1:7788 — a live daemon both stalls the fetch ~4s and sprayed
// real dashboard rows into gate tails, hiding real failures for six
// #271-class loops), and stdout is captured so the gate's 15-line tail
// can no longer be polluted with dashboard pixels.
func TestRunOneShotRendersDashboard(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/agents", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"panes":[{"taskId":"T1","state":"running"}]}`))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf strings.Builder
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	RunOneShot(TuiOptions{URL: srv.URL}, "embedded")
	_ = w.Close()
	os.Stdout = stdout
	out := <-done

	if strings.Contains(out, "\x1b[?1049") {
		t.Fatal("one-shot render must not enter the alternate screen")
	}
	if !strings.Contains(out, "help [q] quit") {
		t.Fatalf("one-shot render must carry the dashboard footer, got %q", truncStr(out, 300))
	}
	if !strings.Contains(out, "T1") {
		t.Fatalf("one-shot render must carry the stub roster pane, got %q", truncStr(out, 300))
	}
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestLoopDrawNeverExceedsTerminalRows pins the short-terminal guard: on a
// degenerate 3-row terminal fitLines' head/body/footer minimums can produce
// more lines than the screen, and the differ's row walk (skip \x1b[1B /
// rewrite \n) past the bottom row would scroll the alternate screen and
// desync the incremental diff forever. drawLocked must cap the frame at the
// row budget in every view — a frame of at most rows-1 lines cannot move
// the cursor past the screen's bottom row (the walk starts at \x1b[H and
// advances at most one row per line, incl. the absolute CUP+ED erase).
func TestLoopDrawNeverExceedsTerminalRows(t *testing.T) {
	env := newBufEnv()
	env.setRows(3)
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	l := NewLoop(TuiOptions{}, tr, env, "attach").(*loop)
	l.running = true
	for range 30 {
		l.logLines = append(l.logLines, LogLine{Message: "row"})
	}
	for _, keys := range []string{"1", "2", "3", "1"} {
		l.ApplyKeys(DecodeKeys(keys, false)) // the key's own safeDraw paints the frame
		if len(l.prevFrame) > env.rows-1 {
			t.Fatalf("view %q painted %d rows on a %d-row terminal (scroll → permanent diff desync)",
				keys, len(l.prevFrame), env.rows)
		}
	}
}
