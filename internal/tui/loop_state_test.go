package tui

// Golden-style state-machine tests for the interactive loop (issue #252):
// ApplyKeys transitions with no terminal involved — the Loop seam keeps the
// loop testable without a PTY. Mirrors the TS handleKey transition table.
import (
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

// newTestLoop builds a loop over a no-op env; the state tests never draw.
func newTestLoop(t *testing.T, opts TuiOptions) *loop {
	t.Helper()
	l := NewLoop(opts, &countingTransport{}, nullEnv{}, "attach").(*loop)
	return l
}

// TestApplyKeysViewSwitching pins the 1/2/3 and s/l view toggles.
func TestApplyKeysViewSwitching(t *testing.T) {
	l := newTestLoop(t, TuiOptions{})
	if l.view != ViewWorkers {
		t.Fatalf("initial view = %v, want workers", l.view)
	}
	l.ApplyKeys(DecodeKeys("2", false))
	if l.view != ViewSessions {
		t.Fatalf("after '2' view = %v, want sessions", l.view)
	}
	l.ApplyKeys(DecodeKeys("s", false))
	if l.view != ViewWorkers {
		t.Fatalf("after 's' view = %v, want workers (toggle back)", l.view)
	}
	l.ApplyKeys(DecodeKeys("l", false))
	if l.view != ViewLog {
		t.Fatalf("after 'l' view = %v, want log", l.view)
	}
	l.ApplyKeys(DecodeKeys("3", false))
	if l.view != ViewLog {
		t.Fatalf("after '3' view = %v, want log", l.view)
	}
	l.ApplyKeys(DecodeKeys("1", false))
	if l.view != ViewWorkers {
		t.Fatalf("after '1' view = %v, want workers", l.view)
	}
}

// TestApplyKeysQuit pins q and Ctrl+C; the keys after the quit key in one
// burst ('1qj') must not mutate state (the TS stopped guard).
func TestApplyKeysQuit(t *testing.T) {
	l := newTestLoop(t, TuiOptions{})
	if quit := l.ApplyKeys(DecodeKeys("q", false)); !quit {
		t.Fatal("q must report quit")
	}
	l = newTestLoop(t, TuiOptions{})
	if quit := l.ApplyKeys(DecodeKeys("\x03", false)); !quit {
		t.Fatal("Ctrl+C must report quit")
	}
	l = newTestLoop(t, TuiOptions{})
	if quit := l.ApplyKeys(DecodeKeys("1qj", false)); !quit {
		t.Fatal("burst containing q must report quit")
	}
	if l.view != ViewWorkers {
		t.Fatalf("post-quit key mutated view: %v", l.view)
	}
}

// TestApplyKeysSelectionNavigation pins arrows/pgup/pgdn/home/end over the
// workers list and the clamp on roster shrink.
func TestApplyKeysSelectionNavigation(t *testing.T) {
	l := newTestLoop(t, TuiOptions{})
	l.snap = &Snapshot{
		Agents: &AgentPayload{Panes: []TuiPane{
			{TaskID: "T1", PaneID: "p1"}, {TaskID: "T2", PaneID: "p2"}, {TaskID: "T3", PaneID: "p3"},
		}},
	}
	l.ApplyKeys(DecodeKeys("\x1b[B", false)) // down
	l.ApplyKeys(DecodeKeys("\x1b[B", false))
	if l.selection != 2 {
		t.Fatalf("after 2×down selection = %d, want 2", l.selection)
	}
	l.ApplyKeys(DecodeKeys("\x1b[B", false)) // clamped at last item
	if l.selection != 2 {
		t.Fatalf("down past end selection = %d, want 2", l.selection)
	}
	l.ApplyKeys(DecodeKeys("\x1b[A", false)) // up
	if l.selection != 1 {
		t.Fatalf("after up selection = %d, want 1", l.selection)
	}
	l.ApplyKeys(DecodeKeys("\x1b[6~", false)) // pgdn → last
	if l.selection != 2 {
		t.Fatalf("pgdn selection = %d, want 2", l.selection)
	}
	l.ApplyKeys(DecodeKeys("\x1b[5~", false)) // pgup → first
	if l.selection != 0 {
		t.Fatalf("pgup selection = %d, want 0", l.selection)
	}
	l.ApplyKeys(DecodeKeys("\x1b[F", false)) // end
	if l.selection != 2 {
		t.Fatalf("end selection = %d, want 2", l.selection)
	}
	l.ApplyKeys(DecodeKeys("\x1b[H", false)) // home
	if l.selection != 0 {
		t.Fatalf("home selection = %d, want 0", l.selection)
	}
}

// TestApplyKeysClampOnShrink: a view switch onto a smaller roster clamps the
// cursor (TS clampSelection).
func TestApplyKeysClampOnShrink(t *testing.T) {
	l := newTestLoop(t, TuiOptions{})
	l.snap = &Snapshot{
		Agents: &AgentPayload{
			Panes:  []TuiPane{{TaskID: "T1"}, {TaskID: "T2"}, {TaskID: "T3"}},
			Queued: []TuiQueuedTask{{ID: "Q1"}, {ID: "Q2"}},
		},
	}
	l.ApplyKeys(DecodeKeys("\x1b[B\x1b[B\x1b[B\x1b[B", false)) // down ×4 → last of 5 items
	if l.selection != 4 {
		t.Fatalf("workers selection = %d, want 4", l.selection)
	}
	l.ApplyKeys(DecodeKeys("2", false)) // sessions view: 3 items
	if l.selection != 2 {
		t.Fatalf("after view switch selection = %d, want clamped 2", l.selection)
	}
}

// TestApplyKeysOverlays pins esc/overlay interplay: `u` opens the upgrade
// overlay, any key closes it; esc closes help first, then nothing else.
func TestApplyKeysOverlays(t *testing.T) {
	l := newTestLoop(t, TuiOptions{})
	l.ApplyKeys(DecodeKeys("u", false))
	if l.overlay == nil || l.overlay.Kind != "upgrade" {
		t.Fatalf("after u overlay = %+v, want upgrade", l.overlay)
	}
	l.ApplyKeys(escResult())
	if l.overlay != nil {
		t.Fatalf("esc must close overlay, got %+v", l.overlay)
	}
	// `?` toggles help; esc closes it.
	l.ApplyKeys(DecodeKeys("?", false))
	if !l.showHelp {
		t.Fatal("? must open help")
	}
	l.ApplyKeys(escResult())
	if l.showHelp {
		t.Fatal("esc must close help")
	}
	// A non-esc key with an overlay open also closes it (TS: overlay = null).
	l.ApplyKeys(DecodeKeys("u", false))
	l.ApplyKeys(DecodeKeys("x", false))
	if l.overlay != nil {
		t.Fatalf("any key must close overlay, got %+v", l.overlay)
	}
}

// TestApplyKeysDetailOverlay pins Enter/o opening the detail panel for the
// selected item, and `nothing selected` when the list is empty.
func TestApplyKeysDetailOverlay(t *testing.T) {
	l := newTestLoop(t, TuiOptions{})
	l.snap = &Snapshot{Agents: &AgentPayload{Panes: []TuiPane{{TaskID: "T1", PaneID: "p1"}}}}
	l.ApplyKeys(DecodeKeys("\r", false))
	if l.overlay == nil || l.overlay.Kind != "detail" || l.overlay.Pane == nil || l.overlay.Pane.TaskID != "T1" {
		t.Fatalf("enter overlay = %+v, want detail of T1", l.overlay)
	}
	// Queued rows detail too.
	l = newTestLoop(t, TuiOptions{})
	l.snap = &Snapshot{Agents: &AgentPayload{Queued: []TuiQueuedTask{{ID: "Q1", Title: "do it"}}}}
	l.ApplyKeys(DecodeKeys("\x1b[B", false)) // onto the queued row
	l.ApplyKeys(DecodeKeys("o", false))
	if l.overlay == nil || l.overlay.Kind != "detail" || l.overlay.Queued == nil || l.overlay.Queued.ID != "Q1" {
		t.Fatalf("o overlay = %+v, want detail of Q1", l.overlay)
	}
	// Empty roster: note, no overlay.
	l = newTestLoop(t, TuiOptions{})
	l.ApplyKeys(DecodeKeys("\r", false))
	if l.overlay != nil || l.note != "nothing selected" {
		t.Fatalf("empty roster: overlay=%+v note=%q", l.overlay, l.note)
	}
}

// TestApplyKeysKillFlow pins the capability gate, the pendingKill confirm,
// the y POST (answer __kill__) and the cancel path.
func TestApplyKeysKillFlow(t *testing.T) {
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	l := NewLoop(TuiOptions{RepoPath: "/repo"}, tr, nullEnv{}, "attach").(*loop)
	l.snap = &Snapshot{
		Status: &StatusPayload{Capabilities: []string{"kill-via-answer"}},
		Agents: &AgentPayload{Panes: []TuiPane{{TaskID: "T9", State: "running", PaneID: "p"}}},
	}
	l.ApplyKeys(DecodeKeys("k", false))
	if l.pendingKill != "T9" {
		t.Fatalf("pendingKill = %q, want T9", l.pendingKill)
	}
	l.ApplyKeys(DecodeKeys("y", false))
	if l.pendingKill != "" {
		t.Fatalf("y must clear pendingKill, got %q", l.pendingKill)
	}
	// 10s budget matches runWaitFor: the POST fires in microseconds when
	// healthy — under `go test ./...` parallel-package load the spawned
	// goroutine can be starved well past 2s (the 4.146s evidence-era tui
	// package failure fits only this sub-5s deadline; issue #271 class).
	select {
	case <-tr.killDone:
	case <-time.After(10 * time.Second):
		t.Fatal("kill POST never ran")
	}
	// The kill goroutine's last act is the mu-held note write (executeKill
	// runs after the POST returns); waiting on it here orders this test's
	// unsynchronized l.note reads after that write instead of racing it —
	// the same discipline the Run-driven tests' conds use.
	runWaitFor(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.note != "killing T9…"
	}, "kill goroutine never finished")
	if tr.kills != 1 || tr.lastKillTask != "T9" {
		t.Fatalf("kill POST = %d (%q), want 1 (T9)", tr.kills, tr.lastKillTask)
	}
	// Cancel: any non-y key.
	l.ApplyKeys(DecodeKeys("k", false))
	l.ApplyKeys(DecodeKeys("n", false))
	if l.pendingKill != "" || l.note != "kill cancelled" {
		t.Fatalf("cancel: pendingKill=%q note=%q", l.pendingKill, l.note)
	}
	if tr.kills != 1 {
		t.Fatalf("cancel must not POST, kills = %d", tr.kills)
	}
	// Ctrl+C cancels the kill instead of quitting (the TS confirm branch
	// consumes every key first).
	l.ApplyKeys(DecodeKeys("k", false))
	if quit := l.ApplyKeys(DecodeKeys("\x03", false)); quit {
		t.Fatal("Ctrl+C during kill confirm must cancel, not quit")
	}
	if tr.kills != 1 || l.pendingKill != "" {
		t.Fatalf("ctrl+c confirm: pendingKill=%q kills=%d", l.pendingKill, tr.kills)
	}
}

// TestApplyKeysKillNotSupported pins the capability gate note.
func TestApplyKeysKillNotSupported(t *testing.T) {
	l := newTestLoop(t, TuiOptions{})
	l.snap = &Snapshot{Status: &StatusPayload{Capabilities: []string{"approve"}}}
	l.ApplyKeys(DecodeKeys("k", false))
	if l.pendingKill != "" || l.note != "kill: not supported by this daemon" {
		t.Fatalf("note=%q pendingKill=%q", l.note, l.pendingKill)
	}
	l.snap = &Snapshot{Status: &StatusPayload{Capabilities: []string{"kill-via-answer"}}}
	l.ApplyKeys(DecodeKeys("k", false))
	if l.note != "kill: no running task" {
		t.Fatalf("empty roster note = %q", l.note)
	}
}

// TestApplyKeysDispatchSheet pins the n sheet: typing appends, backspace
// trims, esc closes, empty Enter refuses, Enter POSTs /dispatch with
// autoPr (FR-HAND-03).
func TestApplyKeysDispatchSheet(t *testing.T) {
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	l := NewLoop(TuiOptions{RepoPath: "/repo"}, tr, nullEnv{}, "attach").(*loop)
	l.ApplyKeys(DecodeKeys("n", false))
	if l.overlay == nil || l.overlay.Kind != "dispatch" {
		t.Fatalf("n overlay = %+v, want dispatch", l.overlay)
	}
	l.ApplyKeys(DecodeKeys("fix", false))
	if l.overlay == nil || l.overlay.Input != "fix" {
		t.Fatalf("typed input = %+v", l.overlay)
	}
	l.ApplyKeys(DecodeKeys("\x7f", false)) // backspace
	if l.overlay == nil || l.overlay.Input != "fi" {
		t.Fatalf("after backspace input = %+v", l.overlay)
	}
	l.ApplyKeys(escResult()) // esc closes
	if l.overlay != nil {
		t.Fatalf("esc must close sheet, got %+v", l.overlay)
	}
	if tr.dispatches != 0 {
		t.Fatalf("esc must not POST, dispatches = %d", tr.dispatches)
	}
	// Empty goal refused.
	l.ApplyKeys(DecodeKeys("n", false))
	l.ApplyKeys(DecodeKeys("\r", false))
	if l.note != "dispatch: empty goal" || tr.dispatches != 0 {
		t.Fatalf("empty goal: note=%q dispatches=%d", l.note, tr.dispatches)
	}
	// Real goal POSTs.
	l.ApplyKeys(DecodeKeys("n", false))
	l.ApplyKeys(DecodeKeys("ship it", false))
	l.ApplyKeys(DecodeKeys("\r", false))
	if tr.dispatches != 1 || tr.lastDispatchPrompt != "ship it" || !tr.lastDispatchAutoPr {
		t.Fatalf("dispatch = %d (%q autoPr=%v)", tr.dispatches, tr.lastDispatchPrompt, tr.lastDispatchAutoPr)
	}
	if l.overlay != nil {
		t.Fatalf("submit must close sheet, got %+v", l.overlay)
	}
}

// TestApplyKeysApproveSheet pins the g sheet: /status.ask picked up,
// y/n shorthand words, free text passthrough, ledger-tail fallback.
func TestApplyKeysApproveSheet(t *testing.T) {
	tr := &countingTransport{killDone: make(chan struct{}, 4)}
	l := NewLoop(TuiOptions{RepoPath: "/repo"}, tr, nullEnv{}, "attach").(*loop)
	// Seeding the transport's canned /status (paused ask kept, Reachable)
	// keeps the test deterministic even if a submit's fire-and-forget poll
	// ever runs: poll refuses to fetch on a loop that never ran — the guard
	// that stopped pre-fix zombie 2s chains outliving ApplyKeys-driven
	// tests and racing later tests' reads (issue #271). The empty-answer
	// refusal runs FIRST — before any submit — so no poll could clobber
	// its note assertion either way.
	tr.snap = &Snapshot{Status: &StatusPayload{Ask: &StatusAsk{ID: "TASK-ask"}}, Reachable: true}
	l.snap = &Snapshot{Status: &StatusPayload{Ask: &StatusAsk{ID: "TASK-ask"}}, Reachable: true}
	// Empty answer refused.
	l.ApplyKeys(DecodeKeys("g", false))
	l.ApplyKeys(DecodeKeys("\r", false))
	if l.note != "approve: empty answer" {
		t.Fatalf("empty answer note = %q", l.note)
	}
	l.ApplyKeys(DecodeKeys("g", false))
	if l.overlay == nil || l.overlay.Kind != "approve" || l.overlay.TaskID != "TASK-ask" {
		t.Fatalf("g overlay = %+v, want approve of TASK-ask", l.overlay)
	}
	l.ApplyKeys(DecodeKeys("y", false))
	l.ApplyKeys(DecodeKeys("\r", false))
	if tr.approves != 1 || tr.lastApproveAnswer != "yes" {
		t.Fatalf("approve = %d answer=%q, want 1 yes", tr.approves, tr.lastApproveAnswer)
	}
	// n → no.
	l.ApplyKeys(DecodeKeys("g", false))
	l.ApplyKeys(DecodeKeys("n", false))
	l.ApplyKeys(DecodeKeys("\r", false))
	if tr.lastApproveAnswer != "no" {
		t.Fatalf("n shorthand answer = %q, want no", tr.lastApproveAnswer)
	}
	// Free text passes through.
	l.ApplyKeys(DecodeKeys("g", false))
	l.ApplyKeys(DecodeKeys("use the", false))
	l.ApplyKeys(DecodeKeys("\r", false))
	if tr.lastApproveAnswer != "use the" {
		t.Fatalf("free-text answer = %q", tr.lastApproveAnswer)
	}
	// Ledger-tail fallback when /status carries no ask.
	l2 := newTestLoop(t, TuiOptions{})
	l2.snap = &Snapshot{History: []HistoryRow{{"taskId": "L1", "verdict": "failed"}, {"taskId": "L2", "verdict": "ask"}}}
	l2.ApplyKeys(DecodeKeys("g", false))
	if l2.overlay == nil || l2.overlay.TaskID != "L2" {
		t.Fatalf("ledger fallback overlay = %+v, want L2", l2.overlay)
	}
	// Without any paused task, g stays jump-to-first.
	l3 := newTestLoop(t, TuiOptions{})
	l3.snap = &Snapshot{Agents: &AgentPayload{Panes: []TuiPane{{TaskID: "T1"}, {TaskID: "T2"}}}}
	l3.ApplyKeys(DecodeKeys("\x1b[B", false))
	l3.ApplyKeys(DecodeKeys("g", false))
	if l3.overlay != nil || l3.selection != 0 {
		t.Fatalf("no paused task: overlay=%+v selection=%d", l3.overlay, l3.selection)
	}
	// A loop that never ran must not poll: submit paths are the only
	// ApplyKeys entry to the transport, and each one is fire-and-forget.
	// Pins poll()'s running-guard so a regression re-opens the zombie-chain
	// race (issue #271) loudly instead of as a CI flake.
	if polls := tr.pollCount(); polls != 0 {
		t.Fatalf("non-Run loop polled %d times, want 0", polls)
	}
}

// TestApplyKeysLogNavigation pins the log-view scroll arithmetic: arrows
// scroll ±1, pgup/pgdn ±10, home jumps to the oldest line, end/esc return
// to follow.
func TestApplyKeysLogNavigation(t *testing.T) {
	l := newTestLoop(t, TuiOptions{})
	for i := 0; i < 25; i++ {
		l.logLines = append(l.logLines, LogLine{Message: "line"})
	}
	l.ApplyKeys(DecodeKeys("3", false))
	l.ApplyKeys(DecodeKeys("\x1b[A", false)) // up: scroll back 1
	if l.logScroll != 1 || l.logFollow {
		t.Fatalf("up: scroll=%d follow=%v", l.logScroll, l.logFollow)
	}
	l.ApplyKeys(DecodeKeys("\x1b[5~", false)) // pgup: +10 → 11
	if l.logScroll != 11 {
		t.Fatalf("pgup scroll = %d, want 11", l.logScroll)
	}
	l.ApplyKeys(DecodeKeys("\x1b[6~", false)) // pgdn: -10 → 1
	if l.logScroll != 1 {
		t.Fatalf("pgdn scroll = %d, want 1", l.logScroll)
	}
	l.ApplyKeys(DecodeKeys("\x1b[H", false)) // home → oldest
	if l.logScroll != 24 {
		t.Fatalf("home scroll = %d, want 24 (len-1)", l.logScroll)
	}
	l.ApplyKeys(DecodeKeys("\x1b[F", false)) // end → follow
	if l.logScroll != 0 || !l.logFollow {
		t.Fatalf("end: scroll=%d follow=%v", l.logScroll, l.logFollow)
	}
	// Esc returns to follow after scrolling.
	l.ApplyKeys(DecodeKeys("\x1b[A\x1b[A\x1b[A", false))
	l.ApplyKeys(escResult())
	if l.logScroll != 0 || !l.logFollow {
		t.Fatalf("esc: scroll=%d follow=%v", l.logScroll, l.logFollow)
	}
}

// TestApplyKeysFollowToggle pins `f` in the log view.
func TestApplyKeysFollowToggle(t *testing.T) {
	l := newTestLoop(t, TuiOptions{})
	l.logLines = []LogLine{{Message: "x"}, {Message: "y"}}
	l.ApplyKeys(DecodeKeys("l", false))
	l.ApplyKeys(DecodeKeys("f", false))
	if l.logFollow {
		t.Fatal("f must unfollow")
	}
	l.ApplyKeys(DecodeKeys("f", false))
	if !l.logFollow {
		t.Fatal("f again must re-follow")
	}
	// f outside the log view is inert.
	l.ApplyKeys(DecodeKeys("1", false))
	l.ApplyKeys(DecodeKeys("f", false))
	if !l.logFollow {
		t.Fatal("f outside log view must not toggle")
	}
}

// TestApplyKeysHelpToggle pins ? toggling in both directions.
func TestApplyKeysHelpToggle(t *testing.T) {
	l := newTestLoop(t, TuiOptions{})
	l.ApplyKeys(DecodeKeys("?", false))
	if !l.showHelp {
		t.Fatal("? must open help")
	}
	l.ApplyKeys(DecodeKeys("?", false))
	if l.showHelp {
		t.Fatal("? again must close help")
	}
}

// TestDecodeKeysPendingRidesAcrossApplyKeys pins the ESC-disambiguation
// contract at the ApplyKeys level: a held partial + flush decodes to Esc
// (close overlay), a full sequence decodes whole.
func TestDecodeKeysPendingRidesAcrossApplyKeys(t *testing.T) {
	l := newTestLoop(t, TuiOptions{})
	l.ApplyKeys(DecodeKeys("u", false))
	if l.overlay == nil {
		t.Fatal("precondition: upgrade overlay open")
	}
	first := DecodeKeys("\x1b", false)
	if first.Pending != "\x1b" || len(first.Keys) != 0 {
		t.Fatalf("bare esc = %+v", first)
	}
	l.ApplyKeys(first) // nothing decoded yet, overlay still open
	if l.overlay == nil {
		t.Fatal("held ESC must not close the overlay before flush")
	}
	l.ApplyKeys(DecodeKeys(first.Pending, true)) // flush → Esc
	if l.overlay != nil {
		t.Fatalf("flushed esc must close overlay, got %+v", l.overlay)
	}
}

// countingTransport is a Transport test double recording the control-plane
// POSTs the loop issues. polls is bumped by the loop's poll goroutine and
// read from test goroutines, so it rides behind a mutex; the POST counters
// are only touched from the goroutine that drives ApplyKeys.
type countingTransport struct {
	kills              int
	lastKillTask       string
	dispatches         int
	lastDispatchPrompt string
	lastDispatchAutoPr bool
	approves           int
	lastApproveTask    string
	lastApproveAnswer  string
	pollMu             sync.Mutex
	polls              int
	killDone           chan struct{}
	// snap overrides the FetchSnapshot answer when set.
	snap *Snapshot
}

func (c *countingTransport) pollCount() int {
	c.pollMu.Lock()
	defer c.pollMu.Unlock()
	return c.polls
}

// escResult is the flushed lone-Esc press (what the loop's 90ms
// disambiguation timer delivers when no sequence follows).
func escResult() DecodeResult {
	return DecodeResult{Keys: []Key{{Kind: KeyEsc}}}
}

func (c *countingTransport) FetchSnapshot(opts TuiOptions) (*Snapshot, error) {
	c.pollMu.Lock()
	c.polls++
	c.pollMu.Unlock()
	if c.snap != nil {
		return c.snap, nil
	}
	return &Snapshot{History: []HistoryRow{}, Reachable: true}, nil
}

func (c *countingTransport) PostJSON(opts TuiOptions, path string, body any) (int, bool, string) {
	switch path {
	case "/approve":
		m := body.(map[string]any)
		if m["answer"] == "__kill__" {
			c.kills++
			c.lastKillTask, _ = m["taskId"].(string)
			c.killDone <- struct{}{}
		} else {
			c.approves++
			c.lastApproveTask, _ = m["taskId"].(string)
			c.lastApproveAnswer, _ = m["answer"].(string)
		}
	case "/dispatch":
		c.dispatches++
		m := body.(map[string]any)
		c.lastDispatchPrompt, _ = m["prompt"].(string)
		c.lastDispatchAutoPr, _ = m["autoPr"].(bool)
	}
	return 200, true, "ok"
}

// nullEnv is the no-op LoopEnv: nothing to draw on, nothing to read.
type nullEnv struct{}

func (nullEnv) Out() io.Writer { return io.Discard }
func (nullEnv) In() io.Reader  { return ioReaderStub{} }

// ioReaderStub is an always-empty reader (nothing to read in state tests).
type ioReaderStub struct{}

func (ioReaderStub) Read([]byte) (int, error)            { return 0, nil }
func (nullEnv) Size() (int, int)                         { return DefaultRows, DefaultColumns }
func (nullEnv) EnterRaw() error                          { return nil }
func (nullEnv) RestoreTerm() error                       { return nil }
func (nullEnv) SuspendAttach(string, string, string) int { return 1 }
func (nullEnv) Sigint() <-chan os.Signal                 { return nil }
func (nullEnv) Sigwinch() <-chan os.Signal               { return nil }

// Regression (issue #252 bring-up): the ticker calls AggregateStatus on
// every tick with whatever the last poll produced — including a snapshot
// whose Status.Runs/Queue blocks are nil. The old num(status.Runs != nil,
// status.Runs.Active) form evaluated status.Runs.Active unconditionally and
// SIGSEGV'd the whole dashboard (the TS status.runs?.active ?? 0 is safe).
func TestAggregateStatusNilBlocksAreSafe(t *testing.T) {
	if got := AggregateStatus(nil, nil); got != "IDLE" {
		t.Fatalf("nil status = %q, want IDLE", got)
	}
	if got := AggregateStatus(&StatusPayload{}, nil); got != "IDLE" {
		t.Fatalf("empty status = %q, want IDLE", got)
	}
	if got := AggregateStatus(&StatusPayload{Runs: &RunsPayload{}}, nil); got != "IDLE" {
		t.Fatalf("runs block without active = %q, want IDLE", got)
	}
	// The ticker's exact call shape: non-nil status, nil runs/queue, panes.
	if got := AggregateStatus(&StatusPayload{Circuit: "closed"}, []TuiPane{{State: "running"}}); got != "RUNNING" {
		t.Fatalf("running pane = %q, want RUNNING", got)
	}
}
