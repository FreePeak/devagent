package tui

// The interactive raw-mode dashboard loop (FR-GO-13, issue #252): the Go
// port of src/tui/tui.ts runInteractive + runTui. Polls a snapshot on the
// 2s cadence, renders through RenderFrame's incremental diff, feeds stdin
// through DecodeKeys into the handleKey state machine (view switching,
// selection, overlays incl. the FR-HAND-02 dispatch and FR-HAND-07 approve
// sheets, the FR-CTRL-03 kill confirm via POST /approve answer __kill__),
// and quits on q / Ctrl+C restoring the screen (defer, panic included).
//
// Deviations from the TS, both behavioral no-ops: Ctrl+Z arrives as byte
// 0x1a (raw mode has ISIG off) and falls through the TS switch default
// exactly like here — no SIGTSTP handling exists in the TS; SIGWINCH is
// likewise not subscribed in the TS — a reflow repaints on the next frame
// (poll ≤2s) via the width-change full-clear in draw.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FreePeak/devagent/internal/herdr"
	"github.com/FreePeak/devagent/internal/ledger"
)

// StdTransport is the concrete Transport over the FR-CTRL daemon API: the
// package-level HTTP+SSE client (transport.go/fetch.go).
type StdTransport struct{}

// FetchSnapshot implements Transport.
func (StdTransport) FetchSnapshot(opts TuiOptions) (*Snapshot, error) {
	return FetchSnapshot(opts), nil
}

// PostJSON implements Transport.
func (StdTransport) PostJSON(opts TuiOptions, path string, body any) (status int, ok bool, note string) {
	r := PostJSON(opts, path, body)
	return r.Status, r.OK, r.Note
}

// loop is the only Loop implementation. All state is guarded by mu; the
// interactive loop itself runs single-threaded over a small set of
// goroutines (input, ticker, poll scheduler) that all funnel mutations
// through the same lock, mirroring the TS event loop's serial handleKey.
type loop struct {
	opts       TuiOptions
	tr         Transport
	env        LoopEnv
	daemonMode string // 'attach' | 'embedded' (header cue)

	mu             sync.Mutex
	stopped        bool
	quitOnce       sync.Once
	quitCh         chan struct{}
	view           View
	showHelp       bool
	selection      int
	overlay        *Overlay
	pendingKill    string
	note           string
	snap           *Snapshot
	pendingInput   string
	samples        []float64
	spinnerFrame   int
	logLines       []LogLine
	logScroll      int
	logFollow      bool
	logSearch      string // committed / filter ("" = unfiltered)
	logSearchDraft string
	logSearchMode  bool
	logMatchIdx    int // index of the last jumped match in FILTERED space (-1 = none)
	sseState       string
	logSource      string
	lastLogID      int
	prevFrame      []string
	prevWidth      int
	prevTitle      string
	suspended      bool
	running        bool
	polling        bool

	escTimer    *time.Timer
	redrawTimer *time.Timer
	pollTimer   *time.Timer
}

// LoopEnv abstracts the process terminal (and the inline-attach child) so
// the loop is testable without a PTY: loop_run_test.go injects a pipe
// reader, a buffer writer, fixed sizes and a stubbed attach.
type LoopEnv interface {
	// Out is the frame sink (stdout).
	Out() io.Writer
	// In is the raw stdin key source.
	In() io.Reader
	// Size reports the current terminal rows/columns (termRows/termColumns
	// with their 40/100 fallbacks).
	Size() (rows, cols int)
	// EnterRaw switches the terminal to raw mode (setRawMode(true)).
	EnterRaw() error
	// RestoreTerm restores the original cooked mode (setRawMode(false) +
	// PTY teardown). Must be safe to call repeatedly.
	RestoreTerm() error
	// SuspendAttach runs the herdr inline attach child (FR-TUI-06) with
	// inherited stdio and returns its exit code (1 when the child could
	// not start, matching the TS child.on('error') path).
	SuspendAttach(paneID, taskID, repoPath string) int
	// Sigint delivers external SIGINT (kill -INT, PTY teardown); may be nil.
	Sigint() <-chan os.Signal
	// Sigwinch delivers terminal resizes (FR-TUI-P-04); may be nil. On
	// delivery the loop re-probes Size and repaints (one sanctioned full
	// clear on width change).
	Sigwinch() <-chan os.Signal
}

// NewLoop builds the interactive dashboard loop on the Loop seam. daemonMode
// is the header cue ('attach' | 'embedded'). The returned Loop's ApplyKeys
// drives the state machine without any terminal involvement (the
// golden-style tests), and Run drives the full raw-mode session.
func NewLoop(opts TuiOptions, tr Transport, env LoopEnv, daemonMode string) Loop {
	now := time.Now()
	return &loop{
		opts:        opts,
		tr:          tr,
		env:         env,
		quitCh:      make(chan struct{}),
		note:        "connecting…",
		snap:        &Snapshot{History: []HistoryRow{}, FetchedAt: now},
		logFollow:   true,
		logMatchIdx: -1,
		sseState:    "off",
	}
}

// Run drives the alternate-screen session until the operator quits
// (q / Ctrl+C, stdin EOF, or SIGINT). The screen and cursor are restored on
// every exit path, panics included.
func (l *loop) Run() error {
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return errors.New("tui: loop already running")
	}
	l.running = true
	l.mu.Unlock()

	_, cols := l.env.Size()
	l.mu.Lock()
	l.prevWidth = cols
	l.mu.Unlock()

	// Alternate screen + hidden cursor (runTui: \x1b[?1049h\x1b[?25l).
	_, _ = io.WriteString(l.env.Out(), "\x1b[?1049h\x1b[?25l")
	if err := l.env.EnterRaw(); err != nil {
		l.leaveScreen()
		l.mu.Lock()
		l.running = false
		l.mu.Unlock()
		return err
	}

	// Live tail (FR-TUI-03): the daemon's SSE /events stream.
	eventsStop := l.subscribeEvents()

	// One poll immediately, then every PollMs (poll → setTimeout chain).
	// TS `void poll()` — out-of-band; poll re-takes mu itself.
	l.pollNowLocked()
	l.scheduleNextPoll()

	// Spinner ticker: animates only while work is live (RUNNING or a
	// pending kill confirm).
	tickerDone := make(chan struct{})
	go l.tickerLoop(tickerDone)

	// Raw stdin → DecodeKeys → handleKey. EOF/terminal loss quits cleanly.
	inputDone := make(chan struct{})
	go l.inputLoop(inputDone)

	// SIGWINCH (FR-TUI-P-04): re-probe the size and repaint promptly
	// instead of waiting for the next poll. Repeated for every resize.
	if winch := l.env.Sigwinch(); winch != nil {
		go func() {
			for {
				select {
				case <-l.quitCh:
					return
				case _, ok := <-winch:
					if !ok {
						return
					}
					l.mu.Lock()
					if !l.suspended && !l.stopped {
						l.safeDrawLocked()
					}
					l.mu.Unlock()
				}
			}
		}()
	}

	// External SIGINT must quit cleanly even though raw mode turned ISIG
	// off (kill -INT, PTY teardown).
	if sig := l.env.Sigint(); sig != nil {
		go func() {
			_, ok := <-sig
			_ = ok
			l.mu.Lock()
			if !l.suspended && !l.stopped {
				l.quitLocked()
			}
			l.mu.Unlock()
		}()
	}

	<-l.quitCh

	close(tickerDone)
	<-inputDone
	if eventsStop != nil {
		eventsStop()
	}
	l.leaveScreen()
	l.mu.Lock()
	l.running = false
	l.mu.Unlock()
	return nil
}

// leaveScreen restores the shell terminal: cooked mode, cursor shown,
// alternate screen left (runTui's finally: setRawMode(false) +
// '\x1b[?1049l\x1b[?25h').
func (l *loop) leaveScreen() {
	_ = l.env.RestoreTerm()
	_, _ = io.WriteString(l.env.Out(), "\x1b[?1049l\x1b[?25h")
}

// quitLocked stops every timer/subscription and releases Run. TS quit():
// stopped=true, clearTimeout×3, pendingInput=”, removeListener, setRawMode
// (false), pause, restore escapes — the terminal writes live in leaveScreen
// so they also cover the panic path.
func (l *loop) quitLocked() {
	if l.stopped {
		return
	}
	l.stopped = true
	l.pendingInput = ""
	l.stopTimersLocked()
	l.quitOnce.Do(func() { close(l.quitCh) })
}

func (l *loop) stopTimersLocked() {
	for _, t := range []*time.Timer{l.escTimer, l.redrawTimer, l.pollTimer} {
		if t != nil {
			t.Stop()
		}
	}
	l.escTimer, l.redrawTimer, l.pollTimer = nil, nil, nil
}

// subscribeEvents opens the SSE tail; returns the stop func.
func (l *loop) subscribeEvents() func() {
	sub := SubscribeEvents(l.opts, l.onEvent, l.onState)
	return sub.Stop
}

// onEvent mirrors the TS subscribeEvents onEvent callback: skip replayed
// ids, parse, keep the newest runId, cap the ring, follow the tail.
func (l *loop) onEvent(id int, data string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id <= l.lastLogID {
		return // replayed after reconnect
	}
	l.lastLogID = id
	line := ParseLogLine(data)
	if line.RunID != "" {
		l.logSource = line.RunID
	}
	l.logLines = append(l.logLines, line)
	if len(l.logLines) > LogCap {
		l.logLines = l.logLines[len(l.logLines)-LogCap:]
	}
	if l.logFollow {
		l.logScroll = 0
	}
	l.drawSoonLocked()
}

// ApplyKeys feeds one decoded chunk into the loop's state machine and
// reports whether the loop should quit. The trailing partial escape
// sequence (res.Pending) is held for the next chunk — the ESC flush timer
// (applyChunk) force-decodes it as a lone Esc press when nothing follows.
// Safe to call directly (tests drive it without any terminal).
func (l *loop) ApplyKeys(res DecodeResult) (quit bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pendingInput = res.Pending
	for _, key := range res.Keys {
		// A key burst can span the quit key ('1qj'): after quit() the
		// remaining keys must not mutate state or draw.
		if l.stopped {
			return true
		}
		l.handleKeyLocked(key)
	}
	return l.stopped
}

// onState mirrors the SSE connection-state callback (LIVE/RECONNECTING).
func (l *loop) onState(state EventsState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sseState = string(state)
	l.drawSoonLocked()
}

// tickerLoop advances the spinner while the aggregate status is RUNNING or
// a kill confirmation is pending; the incremental renderer makes the 1-line
// header rewrite per tick effectively free.
func (l *loop) tickerLoop(done chan struct{}) {
	t := time.NewTicker(TickerMs * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			l.mu.Lock()
			if l.stopped {
				l.mu.Unlock()
				return
			}
			agg := AggregateStatus(l.snap.Status, rosterPanes(l.snap))
			if agg == "RUNNING" || l.pendingKill != "" {
				l.spinnerFrame = (l.spinnerFrame + 1) % len([]rune(Spinner))
				l.safeDrawLocked()
			}
			l.mu.Unlock()
		}
	}
}

// inputLoop reads raw stdin chunks and drives the state machine. The
// trailing partial escape sequence rides in pendingInput; a lone ESC is
// flushed by the 90ms disambiguation timer (armEscFlush).
func (l *loop) inputLoop(done chan struct{}) {
	defer close(done)
	buf := make([]byte, 4096)
	for {
		n, err := l.env.In().Read(buf)
		if n > 0 {
			l.applyChunk(string(buf[:n]))
		}
		if err != nil {
			// Terminal closed (EOF / PTY teardown): quit cleanly.
			l.mu.Lock()
			l.quitLocked()
			l.mu.Unlock()
			return
		}
		l.mu.Lock()
		stopped := l.stopped
		l.mu.Unlock()
		if stopped {
			return
		}
	}
}

// applyChunk is the TS onData handler: prepend the held partial, decode,
// re-arm or disarm the ESC flush, then feed each key to handleKey.
func (l *loop) applyChunk(chunk string) {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return
	}
	res := DecodeKeys(l.pendingInput+chunk, false)
	l.pendingInput = res.Pending
	if res.Pending != "" {
		l.armEscFlushLocked()
	} else {
		l.disarmEscFlushLocked()
	}
	l.mu.Unlock()
	l.ApplyKeys(res)
}

// Snapshot returns the latest polled snapshot (the render input).
func (l *loop) Snapshot() *Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snap
}

// handleKeyLocked is the TS handleKey transition table (mu held).
func (l *loop) handleKeyLocked(key Key) {
	// Kill confirmation consumes every key first — even Ctrl+C cancels the
	// kill rather than quitting.
	if l.pendingKill != "" {
		if key.Kind == KeyChar && (key.Ch == "y" || key.Ch == "Y") {
			target := l.pendingKill
			l.pendingKill = ""
			l.note = "killing " + target + "…"
			l.safeDrawLocked()
			go l.executeKill(target)
			return
		}
		l.pendingKill = ""
		l.note = "kill cancelled"
		l.safeDrawLocked()
		return
	}
	if key.Kind == KeyCtrl && key.Ch == "\x03" {
		l.quitLocked()
		return
	}
	// Log search prompt (the lazygit/gh-dash convention): printable chars
	// edit the draft live, Backspace trims, Enter applies and jumps to the
	// newest match, Esc cancels. Everything else is swallowed so dashboard
	// hotkeys never fire mid-query.
	if l.logSearchMode {
		switch {
		case key.Kind == KeyEsc:
			l.logSearchMode = false
		case key.Kind == KeyEnter:
			l.logSearchMode = false
			l.logSearch = l.logSearchDraft
			l.logMatchIdx = -1
			if l.logSearch != "" {
				l.jumpLogMatchLocked(1)
			}
		case key.Kind == KeyCtrl && (key.Ch == "\x7f" || key.Ch == "\x08"):
			if r := []rune(l.logSearchDraft); len(r) > 0 {
				l.logSearchDraft = string(r[:len(r)-1])
			}
		case key.Kind == KeyChar:
			l.logSearchDraft += key.Ch
		}
		l.safeDrawLocked()
		return
	}
	// Typed-input overlays (FR-HAND-02/07): printable chars append to the
	// draft, Backspace trims, Enter submits, Esc closes; every other key is
	// swallowed so dashboard hotkeys never fire mid-sentence.
	if l.overlay != nil && (l.overlay.Kind == "dispatch" || l.overlay.Kind == "approve") {
		switch {
		case key.Kind == KeyEsc:
			l.overlay = nil
		case key.Kind == KeyEnter:
			draft := l.overlay
			l.overlay = nil
			l.submitOverlayLocked(draft)
			return
		case key.Kind == KeyCtrl && (key.Ch == "\x7f" || key.Ch == "\x08"):
			ov := *l.overlay
			r := []rune(ov.Input)
			if len(r) > 0 {
				ov.Input = string(r[:len(r)-1])
			}
			l.overlay = &ov
		case key.Kind == KeyChar:
			ov := *l.overlay
			ov.Input += key.Ch
			l.overlay = &ov
		}
		l.safeDrawLocked()
		return
	}
	if key.Kind == KeyEsc {
		if l.overlay != nil {
			l.overlay = nil
		} else if l.showHelp {
			l.showHelp = false
		} else if l.view == ViewLog && l.logSearch != "" {
			// Esc clears an active search before it resets the scroll.
			l.logSearch = ""
			l.logMatchIdx = -1
		} else if l.view == ViewLog && (l.logScroll > 0 || !l.logFollow) {
			l.logScroll = 0
			l.logFollow = true
		}
		l.safeDrawLocked()
		return
	}
	if l.overlay != nil {
		l.overlay = nil
		l.safeDrawLocked()
		return
	}
	ch := ""
	if key.Kind == KeyChar {
		ch = key.Ch
	}
	switch ch {
	case "1":
		l.view = ViewWorkers
		l.clampSelectionLocked()
	case "n":
		// While a log search is active, n/N walk the matches (the lazygit
		// convention); without one, n stays the dispatch sheet.
		if l.view == ViewLog && l.logSearch != "" {
			l.jumpLogMatchLocked(1)
		} else {
			l.overlay = DispatchOverlay()
		}
	case "N":
		if l.view == ViewLog && l.logSearch != "" {
			l.jumpLogMatchLocked(-1)
		}
	case "g":
		// Approve sheet (FR-HAND-07): answer the newest paused 'ask' task.
		// Without a paused task, g stays the jump-to-first binding — and in
		// the log view "first" means the oldest line (the help's "g / G
		// jump to first / last item (log: oldest / newest)" promise; a bare
		// selection=0 there was a silent no-op).
		if paused := pickPausedTask(l.snap); paused != "" {
			l.overlay = ApproveOverlay(paused)
		} else if l.view == ViewLog {
			l.logScroll = maxInt(0, l.visibleLogCount()-1)
			l.logFollow = l.logScroll == 0
		} else {
			l.selection = 0
		}
	case "/":
		if l.view == ViewLog {
			l.logSearchMode = true
			l.logSearchDraft = l.logSearch
		} else {
			l.note = "search: switch to the log view first (3)"
		}
	case "2":
		l.view = ViewSessions
		l.clampSelectionLocked()
	case "3":
		l.view = ViewLog
	case "s":
		if l.view == ViewSessions {
			l.view = ViewWorkers
		} else {
			l.view = ViewSessions
		}
		l.clampSelectionLocked()
	case "l":
		if l.view == ViewLog {
			l.view = ViewWorkers
		} else {
			l.view = ViewLog
		}
	case "r":
		l.pollNowLocked()
		return
	case "k":
		l.beginKillLocked()
	case "a":
		l.attachLocked()
	case "u":
		l.overlay = UpgradeOverlay()
	case "f":
		if l.view == ViewLog {
			l.logFollow = !l.logFollow
			if l.logFollow {
				l.logScroll = 0
			}
		}
	case "?":
		l.showHelp = !l.showHelp
	case "q":
		l.quitLocked()
		return
	default:
	}
	if key.Kind == KeyEnter || ch == "o" || ch == "O" {
		items := viewItems(l.snap, l.view)
		var item any
		if l.selection >= 0 && l.selection < len(items) {
			item = items[l.selection]
		}
		if item != nil {
			switch it := item.(type) {
			case TuiPane:
				l.overlay = DetailOverlay(&it, nil)
			case TuiQueuedTask:
				l.overlay = DetailOverlay(nil, &it)
			}
		} else if l.view != ViewLog {
			l.note = "nothing selected"
		}
		l.safeDrawLocked()
		return
	}
	// Navigation: arrows move the selection (lists) or scroll (log); g/G
	// home/end, PgUp/PgDn page. ('k' stays kill per FR-TUI-05, so lists use
	// arrows — the htop default — instead of vi keys.)
	//
	// Log scroll coordinates live in the SAME filtered space the renderer
	// windows over: with an active search the viewport paginates matches,
	// not the raw buffer.
	down := key.Kind == KeyDown
	up := key.Kind == KeyUp
	if down || up {
		if l.view == ViewLog {
			max := l.visibleLogCount() - 1
			delta := -1
			if up {
				delta = 1
			}
			l.logScroll = minInt(max, maxInt(0, l.logScroll+delta))
			l.logFollow = l.logScroll == 0
		} else {
			n := len(viewItems(l.snap, l.view))
			delta := -1
			if down {
				delta = 1
			}
			l.selection = minInt(maxInt(0, n-1), maxInt(0, l.selection+delta))
		}
		l.safeDrawLocked()
		return
	}
	if key.Kind == KeyPgUp || key.Kind == KeyPgDn {
		if l.view == ViewLog {
			max := l.visibleLogCount() - 1
			delta := -10
			if key.Kind == KeyPgUp {
				delta = 10
			}
			l.logScroll = minInt(max, maxInt(0, l.logScroll+delta))
			l.logFollow = l.logScroll == 0
		} else if key.Kind == KeyPgUp {
			l.selection = 0
		} else {
			l.selection = maxInt(0, len(viewItems(l.snap, l.view))-1)
		}
		l.safeDrawLocked()
		return
	}
	if key.Kind == KeyHome {
		if l.view == ViewLog {
			l.logScroll = maxInt(0, l.visibleLogCount()-1)
			l.logFollow = l.logScroll == 0
		} else {
			l.selection = 0
		}
		l.safeDrawLocked()
		return
	}
	if key.Kind == KeyEnd || ch == "G" {
		if l.view == ViewLog {
			l.logScroll = 0
			l.logFollow = true
		} else {
			l.selection = maxInt(0, len(viewItems(l.snap, l.view))-1)
		}
		l.safeDrawLocked()
		return
	}
	l.safeDrawLocked()
}

// clampSelectionLocked keeps the cursor inside the current view's item list
// visibleLogCount is the log viewport's scroll space: every buffered line
// when unfiltered, only the matches when a search is active.
func (l *loop) visibleLogCount() int {
	if l.logSearch == "" {
		return len(l.logLines)
	}
	return logLinesMatching(&LogViewState{Lines: l.logLines, Search: l.logSearch})
}

// after a view switch or a roster shrink (the TS clampSelection).
func (l *loop) clampSelectionLocked() {
	n := len(viewItems(l.snap, l.view))
	if l.selection >= n {
		l.selection = maxInt(0, n-1)
	}
}

// jumpLogMatchLocked moves the log viewport to the next/previous match of
// the active search. dir=+1 walks toward the tail (newer — n), dir=-1
// toward the head (older — N); both wrap. A cold start (no previous jump)
// lands on the newest match for +1 and the oldest for -1.
//
// All coordinates are in FILTERED space: logViewLines windows over the
// match list when a search is active, computing
// start = len(visible) - viewport - scroll, so pinning match `m` as the
// bottom visible row needs scroll = max(0, len(visible) - 1 - m) in that
// same space. The buffer index is stored at jump time (value-equality
// re-resolution breaks on duplicated raw lines); buffer rolls simply
// invalidate the position and the next jump re-colds-starts.
func (l *loop) jumpLogMatchLocked(dir int) {
	vis := logVisibleLines(&LogViewState{Lines: l.logLines, Search: l.logSearch})
	if len(vis) == 0 {
		l.note = "search: no matches"
		return
	}
	cur := l.logMatchIdx // index into vis from the previous jump
	if cur >= len(vis) {
		cur = -1 // buffer rolled since the last jump
	}
	var next int
	if cur == -1 {
		if dir > 0 {
			next = len(vis) - 1 // newest
		} else {
			next = 0 // oldest
		}
	} else {
		next = (cur + dir + len(vis)) % len(vis)
	}
	l.logMatchIdx = next
	// Scroll in filtered space so the match is the last visible row. With
	// scroll 0 the window is the filtered tail — the newest match — so
	// follow stays on exactly when the pinned match is that tail.
	l.logScroll = maxInt(0, len(vis)-1-next)
	l.logFollow = l.logScroll == 0
}

// beginKillLocked mirrors beginKill: gate on the advertised capability, then
// pick the target (selection, else a running pane, else any pane, else the
// first pending queue row).
func (l *loop) beginKillLocked() {
	caps := []string{}
	if l.snap.Status != nil {
		caps = l.snap.Status.Capabilities
	}
	if !containsStr(caps, "kill-via-answer") {
		l.note = "kill: not supported by this daemon"
		return
	}
	target := PickKillTarget(l.snap, l.view, l.selection)
	if target == "" {
		l.note = "kill: no running task"
		return
	}
	l.pendingKill = target
}

// pickPausedTask mirrors the TS helper: /status.ask carries the board's
// 'ask' task directly; the ledger-tail 'ask' verdict is the fallback. ""
// when nothing is paused.
func pickPausedTask(snap *Snapshot) string {
	if snap.Status != nil && snap.Status.Ask != nil && snap.Status.Ask.ID != "" {
		return snap.Status.Ask.ID
	}
	for i := len(snap.History) - 1; i >= 0; i-- {
		row := snap.History[i]
		taskID, _ := row["taskId"].(string)
		if taskID == "" {
			continue
		}
		if verdict, _ := row["verdict"].(string); verdict == "ask" {
			return taskID
		}
	}
	return ""
}

// executeKill mirrors executeKill: the kill runs through the same gate
// machinery as the CLI (POST /approve with answer __kill__), only when the
// daemon advertises the capability. Never fails into the loop.
func (l *loop) executeKill(taskID string) {
	caps := []string{}
	l.mu.Lock()
	snap := l.snap
	l.mu.Unlock()
	if snap.Status != nil {
		caps = snap.Status.Capabilities
	}
	if !containsStr(caps, "kill-via-answer") {
		l.mu.Lock()
		l.note = "kill: not supported by this daemon"
		l.safeDrawLocked()
		l.mu.Unlock()
		return
	}
	_, ok, note := l.tr.PostJSON(l.opts, "/approve", map[string]any{
		"repoPath": l.repoPath(),
		"taskId":   taskID,
		"answer":   "__kill__",
	})
	var msg string
	if ok {
		msg = fmt.Sprintf("kill: %s accepted (%s)", taskID, note)
	} else {
		msg = fmt.Sprintf("kill %s failed: %s", taskID, note)
	}
	l.mu.Lock()
	l.note = msg
	l.safeDrawLocked()
	l.mu.Unlock()
}

// submitOverlayLocked posts a typed overlay draft to the daemon's control
// plane (the TS submitOverlay).
func (l *loop) submitOverlayLocked(draft *Overlay) {
	switch draft.Kind {
	case "dispatch":
		goal := strings.TrimSpace(draft.Input)
		if goal == "" {
			l.note = "dispatch: empty goal"
			return
		}
		l.note = "dispatching goal…"
		l.safeDrawLocked()
		// autoPr defaults on (FR-HAND-03): with GITHUB_TOKEN the dispatched
		// task publishes a PR after gates; without it the run stays local.
		_, ok, note := l.tr.PostJSON(l.opts, "/dispatch", map[string]any{
			"prompt":   goal,
			"repoPath": l.repoPath(),
			"autoPr":   true,
		})
		if ok {
			l.note = "goal dispatched — watch the workers view"
		} else {
			l.note = clipNote("dispatch failed: " + note)
		}
		l.pollNowLocked()
	case "approve":
		taskID := draft.TaskID
		raw := strings.TrimSpace(draft.Input)
		if raw == "" {
			l.note = "approve: empty answer"
			return
		}
		// y/n shorthand → approve/deny wording; anything else is the
		// free-text answer the worker resumes with (open question 2 in #145).
		answer := raw
		switch strings.ToLower(raw) {
		case "y":
			answer = "yes"
		case "n":
			answer = "no"
		}
		_, ok, note := l.tr.PostJSON(l.opts, "/approve", map[string]any{
			"repoPath": l.repoPath(),
			"taskId":   taskID,
			"answer":   answer,
		})
		if ok {
			l.note = "answered " + taskID
		} else {
			l.note = clipNote("approve failed: " + note)
		}
		l.pollNowLocked()
	}
}

// attachLocked mirrors the `a` case (FR-TUI-06): suspend the dashboard, hand
// the terminal to the selected pane's herdr attach child, restore on detach.
func (l *loop) attachLocked() {
	if l.view == ViewLog {
		l.note = "attach: switch to workers or sessions first"
		return
	}
	items := viewItems(l.snap, l.view)
	var pane *TuiPane
	if l.selection >= 0 && l.selection < len(items) {
		if p, ok := items[l.selection].(TuiPane); ok {
			pane = &p
		}
	}
	if pane == nil {
		l.note = "attach: select a worker or session pane"
		return
	}
	// A burst like 'aa' must not queue a second attach behind the first
	// (the second 'a' runs after this await returns and would re-attach).
	if l.suspended {
		return
	}
	l.suspended = true
	l.pendingInput = "" // a held partial must not leak into the child
	l.disarmEscFlushLocked()
	// Suspend = leave the alternate screen + show cursor + drain raw mode,
	// so the child owns a clean interactive terminal.
	_ = l.env.RestoreTerm()
	_, _ = io.WriteString(l.env.Out(), "\x1b[?1049l\x1b[?25h")
	// FR-TUI-P-01: the child dying — nonzero exit, signal death, or a panic
	// inside the env — must never process-exit the dashboard. Every path
	// below runs the same resume: re-enter the alternate screen, redraw from
	// scratch, re-arm raw input, refresh.
	code := 1
	attachNote := ""
	func() {
		defer func() {
			if r := recover(); r != nil {
				attachNote = fmt.Sprintf("attach crashed: %v", r)
			}
		}()
		code = l.env.SuspendAttach(pane.PaneID, pane.TaskID, l.repoPath())
	}()
	_, _ = io.WriteString(l.env.Out(), "\x1b[?1049h\x1b[?25l")
	_ = l.env.EnterRaw()
	l.suspended = false
	l.prevFrame = nil
	switch {
	case code == 0:
		l.note = "detached from " + pane.TaskID
	case code < 0:
		l.note = fmt.Sprintf("attach child died (signal %d)", -code)
	case attachNote != "":
		l.note = attachNote
	default:
		l.note = fmt.Sprintf("attach exited (%d)", code)
	}
	l.safeDrawLocked()
	l.pollNowLocked()
}

// pollNowLocked schedules the TS `void poll()`: fire-and-forget, out of the
// key critical section (poll re-takes mu itself, so a locked caller must
// not invoke it synchronously).
func (l *loop) pollNowLocked() {
	if l.stopped {
		return
	}
	go l.poll()
}

// poll mirrors poll(): guarded, then fetch → note → clamp → sparkline
// sample → draw, and reschedules itself unless stopped.
func (l *loop) poll() {
	l.mu.Lock()
	if l.polling || l.stopped {
		l.mu.Unlock()
		return
	}
	l.polling = true
	l.mu.Unlock()

	next, err := l.tr.FetchSnapshot(l.opts)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.polling = false
	if l.stopped {
		return
	}
	if err != nil {
		// Never let a poll failure become a crash: show it, keep looping.
		l.note = clipNote("internal: " + err.Error())
		l.safeDrawLocked()
		l.scheduleNextPollLocked()
		return
	}
	l.snap = next
	if next.Reachable {
		l.note = ""
	} else {
		l.note = "daemon unreachable — retrying"
	}
	l.clampSelectionLocked()
	// Sparkline sample: the truthier of running panes and run-registry
	// locks.
	running := 0
	for _, p := range rosterPanes(next) {
		if p.State == "running" {
			running++
		}
	}
	active := float64(running)
	if next.Status != nil && next.Status.Runs != nil && next.Status.Runs.Active != nil &&
		*next.Status.Runs.Active > active {
		active = *next.Status.Runs.Active
	}
	l.samples = append(l.samples, active)
	if len(l.samples) > SparkSamples {
		l.samples = l.samples[1:]
	}
	l.safeDrawLocked()
	l.scheduleNextPollLocked()
}

// scheduleNextPoll arms the 2s chain (the TS setTimeout in poll's tail).
func (l *loop) scheduleNextPoll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.scheduleNextPollLocked()
}

func (l *loop) scheduleNextPollLocked() {
	if l.stopped || l.pollTimer != nil {
		return
	}
	l.pollTimer = time.AfterFunc(PollMs*time.Millisecond, func() {
		l.mu.Lock()
		l.pollTimer = nil
		l.mu.Unlock()
		l.poll()
	})
}

// armEscFlushLocked holds a lone Esc press for ESC_FLUSH_MS so an
// arrow-style sequence split across chunks still decodes whole.
func (l *loop) armEscFlushLocked() {
	if l.escTimer != nil {
		return
	}
	l.escTimer = time.AfterFunc(escFlushInterval, func() {
		l.mu.Lock()
		l.escTimer = nil
		if l.stopped {
			l.pendingInput = ""
			l.mu.Unlock()
			return
		}
		if l.suspended {
			l.pendingInput = "" // attach child owns the terminal; drop the partial
			l.mu.Unlock()
		}
		partial := l.pendingInput
		l.pendingInput = ""
		l.mu.Unlock()
		l.ApplyKeys(DecodeKeys(partial, true))
	})
}

func (l *loop) disarmEscFlushLocked() {
	if l.escTimer != nil {
		l.escTimer.Stop()
		l.escTimer = nil
	}
}

// escFlushInterval is the ESC-disambiguation window (TS ESC_FLUSH_MS = 90).
const escFlushInterval = 90 * time.Millisecond

// drawSoonLocked coalesces SSE-driven redraws into one frame per 250ms.
func (l *loop) drawSoonLocked() {
	if l.redrawTimer != nil || l.stopped {
		return
	}
	l.redrawTimer = time.AfterFunc(250*time.Millisecond, func() {
		l.mu.Lock()
		l.redrawTimer = nil
		if !l.stopped {
			l.safeDrawLocked()
		}
		l.mu.Unlock()
	})
}

// drawLocked renders one frame through the incremental diff. A width change
// (SIGWINCH picked up on the next frame) needs one full clear first.
func (l *loop) drawLocked() {
	if l.stopped || l.suspended {
		return
	}
	rows, width := l.env.Size()
	rows = maxInt(12, rows-1) // headroom: never scroll
	if width != l.prevWidth {
		l.prevWidth = width
		l.prevFrame = nil
		_, _ = io.WriteString(l.env.Out(), "\x1b[H\x1b[2J") // a reflow needs one full clear
	}
	scroll := l.logScroll
	if l.logFollow {
		scroll = 0
	}
	next := RenderLines(l.snap, RenderOptions{
		View:        l.view,
		ShowHelp:    l.showHelp,
		DaemonMode:  l.daemonMode,
		Selection:   l.selection,
		Overlay:     l.overlay,
		Note:        l.note,
		PendingKill: l.pendingKill,
		Metrics:     &MetricsState{Samples: l.samples, SampleMs: PollMs},
		Log: &LogViewState{Lines: l.logLines, Scroll: scroll, Follow: l.logFollow,
			State: l.sseState, Source: l.logSource, Search: l.logSearch,
			SearchDraft: l.logSearchDraft, SearchMode: l.logSearchMode},
		Rows:         rows,
		Width:        width,
		SpinnerFrame: l.spinnerFrame,
	})
	title := "devagent — " + l.aggregateTitleLocked()
	if title != l.prevTitle {
		// OSC 2 window title (k9s convention): the dashboard state stays
		// glanceable in the tab/window manager without stealing a row.
		_, _ = io.WriteString(l.env.Out(), "\x1b]2;"+title+"\x07")
		l.prevTitle = title
	}
	_, _ = io.WriteString(l.env.Out(), RenderFrame(l.prevFrame, next, width)+"\n")
	l.prevFrame = next
}

// aggregateTitleLocked names the terminal-title aggregate: the status the
// header chip shows, or a short "offline" cue when the daemon is down.
func (l *loop) aggregateTitleLocked() string {
	if l.snap == nil || !l.snap.Reachable || l.snap.Status == nil {
		return "offline"
	}
	return AggregateStatus(l.snap.Status, rosterPanes(l.snap))
}

// safeDrawLocked keeps a render failure from taking the app down (a crash
// on the alternate screen leaves the operator's terminal broken): surface
// it in the footer's note and keep running.
func (l *loop) safeDrawLocked() {
	if l.stopped || l.suspended {
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				l.note = clipNote(fmt.Sprintf("internal: %v", r))
			}
		}()
		l.drawLocked()
	}()
	// The next poll/tick retries with fresh state; one guarded retry here
	// gets the note on screen immediately.
	if !l.stopped && !l.suspended {
		func() {
			defer func() { _ = recover() }()
			l.drawLocked()
		}()
	}
}

// repoPath resolves the repo echoed into kill (approve) calls
// (opts.repoPath ?? process.cwd()).
func (l *loop) repoPath() string {
	if l.opts.RepoPath != "" {
		return l.opts.RepoPath
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// clipNote mirrors the TS `.slice(0, 80)` on operator-facing notes.
func clipNote(s string) string {
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:80])
	}
	return s
}

// TermEnv is the production LoopEnv over the real process terminal.
type TermEnv struct {
	term *rawTerm
}

// NewTermEnv probes stdin/stdout/stderr for the descriptor that owns the
// keyboard and captures its original termios. nil term = no TTY anywhere
// (piped in/out): the interactive loop then refuses to start (errNoTTY) —
// the caller's non-TTY branch never reaches it.
func NewTermEnv() *TermEnv {
	for _, fd := range []int{0, 1, 2} {
		if t, ok := rawProbe(fd); ok {
			return &TermEnv{term: t}
		}
	}
	return &TermEnv{}
}

// errNoTTY is returned when no descriptor carries the keyboard (raw mode
// is impossible — the TS setRawMode would throw).
var errNoTTY = errors.New("stdin is not a terminal; raw mode unavailable")

// StdinIsTTY mirrors process.stdin.isTTY: a TCGETS probe on fd 0 (isatty),
// not the Stat char-device heuristic (which false-positives on /dev/null).
func StdinIsTTY() bool {
	_, ok := rawProbe(0)
	return ok
}

func (e *TermEnv) Out() io.Writer { return os.Stdout }

func (e *TermEnv) In() io.Reader { return os.Stdin }

// Size reports the current terminal geometry with the TS fallbacks
// (termRows/termColumns: 40 rows × 100 cols when the ioctl has nothing).
func (e *TermEnv) Size() (rows, cols int) { return termSize(1) }

func (e *TermEnv) EnterRaw() error {
	if e.term == nil {
		return errNoTTY
	}
	return e.term.enterRaw()
}

func (e *TermEnv) RestoreTerm() error {
	if e.term != nil {
		e.term.restore()
	}
	return nil
}
func (e *TermEnv) Sigint() <-chan os.Signal {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT)
	return sig
}

// SuspendAttach runs `herdr --session <s> agent attach <paneId>` with
// inherited stdio, exactly what `devagent attach <task> --exec` does
// (FR-VIS-02). The resolved pane id is recorded in the orchestration ledger
// (operator-attached) like the CLI path. Exit code 1 when the child cannot
// start (the TS child.on('error') path).
func (e *TermEnv) SuspendAttach(paneID, taskID, repoPath string) int {
	session := herdr.ResolveSession("")
	ledger.AppendOperatorAttachRecord(repoPath, ledger.OperatorAttachRecord{
		TS:      ledger.NowISO(),
		Kind:    "event",
		Event:   "operator-attached",
		TaskID:  taskID,
		Attempt: 1,
		PaneID:  paneID,
		Session: session,
	})
	cmd := exec.Command(herdr.HerdrBin(), "--session", session, "agent", "attach", paneID)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

// ProbeDaemon is the unauthenticated liveness probe (the daemon's
// /healthz). Never fails. The attach-vs-embed decision (ensureDaemon) lives
// with the CLI command — the tui package is a pure client of the FR-CTRL
// daemon API.
func ProbeDaemon(opts TuiOptions) bool {
	return DaemonRequest(opts, "GET", "/healthz", "", 900*time.Millisecond).Status == 200
}

// RunOneShot is the non-TTY degrade path (TS runOneShot): one snapshot
// render to stdout, exit 0. The daemon resolution (attach or embed) stays
// with the caller (internal/cli) — the tui package is a pure client.
func RunOneShot(opts TuiOptions, daemonMode string) {
	snap, _ := StdTransport{}.FetchSnapshot(opts)
	_, _ = fmt.Fprintln(os.Stdout, RenderDashboard(snap, RenderOptions{DaemonMode: daemonMode}))
}
