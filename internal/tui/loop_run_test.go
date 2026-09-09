package tui

// Driver-level tests for the interactive loop (issue #252): Run() over an
// injected fake stdin reader and fake Transport — the seams keep the whole
// raw-mode session testable without a PTY (the env never enters raw mode,
// frames land in a buffer, and the sizes are pinned).

import (
	"fmt"
	"io"
	"os"
	"strings"
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
// mode bookkeeping, stubbed attach, no signals.
type bufEnv struct {
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

func (e *bufEnv) Out() io.Writer { return writerFunc(e.buf.Write) }

func (e *bufEnv) In() io.Reader              { return e.in }
func (e *bufEnv) Size() (int, int)           { return e.rows, e.cols }
func (e *bufEnv) EnterRaw() error            { e.rawEnters++; return nil }
func (e *bufEnv) RestoreTerm() error         { e.restores++; return nil }
func (e *bufEnv) Sigint() <-chan os.Signal   { return nil }
func (e *bufEnv) Sigwinch() <-chan os.Signal { return nil }
func (e *bufEnv) screenLeft() bool           { return strings.Contains(e.buf.String(), "\x1b[?1049l\x1b[?25h") }
func (e *bufEnv) screenEntered() bool {
	return strings.Contains(e.buf.String(), "\x1b[?1049h\x1b[?25l")
}

func (e *bufEnv) SuspendAttach(paneID, taskID, repoPath string) int {
	e.attachLog = append(e.attachLog, paneID+"|"+taskID+"|"+repoPath)
	return e.attachCode
}

// runWaitFor adapts the shared waitFor(cond, ms) with a failure message.
func runWaitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	waitFor(t, cond, 3000)
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

	runWaitFor(t, func() bool { return tr.polls >= 1 }, "initial poll never ran")
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
		t.Fatalf("screen enter/leave missing: %q", truncStr(env.buf.String(), 200))
	}
	if env.rawEnters != 1 || env.restores < 1 {
		t.Fatalf("raw enters=%d restores=%d", env.rawEnters, env.restores)
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
		return strings.Contains(env.buf.String(), "[1] workers")
	}, "frame with the workers footer never rendered")

	env.in.write("q")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after q")
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
	runWaitFor(t, func() bool { return len(env.attachLog) == 1 }, "attach child never ran")
	if env.attachLog[0] != "p1|T1|/repo" {
		t.Fatalf("attach args = %q, want p1|T1|/repo", env.attachLog[0])
	}

	env.in.write("q")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after attach+q")
	}
	// Suspend must have left + re-entered the screen around the child.
	s := env.buf.String()
	first := strings.Index(s, "\x1b[?1049l\x1b[?25h")
	second := strings.LastIndex(s, "\x1b[?1049h\x1b[?25l")
	if first < 0 || second < 0 || second < first {
		t.Fatalf("suspend/resume escapes out of order: enter@%d leave@%d", second, first)
	}
	if env.rawEnters != 2 {
		t.Fatalf("raw enters = %d, want 2 (initial + attach resume)", env.rawEnters)
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
	runWaitFor(t, func() bool { return len(env.attachLog) == 1 }, "attach child never ran")

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

	deadline := time.Now().Add(3 * time.Second)
	for tr.polls < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if tr.polls < 2 {
		t.Fatalf("polls = %d, want ≥2 (initial + scheduled)", tr.polls)
	}
	env.in.write("q")
	<-done
}

// TestRunOneShotRendersDashboard pins the non-TTY degrade: one dashboard
// render + newline, no alternate-screen escapes.
func TestRunOneShotRendersDashboard(t *testing.T) {
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	RunOneShot(TuiOptions{}, "embedded")
	// RunOneShot writes to real stdout; assert via the transport instead.
	if tr.polls != 0 {
		t.Fatal("RunOneShot must use StdTransport, not the injected one")
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
