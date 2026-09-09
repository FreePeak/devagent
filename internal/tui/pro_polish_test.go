package tui

// Contract tests for the professional-polish wave (conventions researched
// from k9s, lazygit, gh-dash, htop, opencode, crush): mono/NO_COLOR
// degradation, the PAUSED aggregate + hero attention banner, log search
// (jump math in FILTERED space), selection-following viewports, and the
// OSC-2 window title.

import (
	"strings"
	"testing"
)

// --- mono degradation (NO_COLOR / TERM=dumb) -------------------------------

func TestMonoFor(t *testing.T) {
	if monoFor(func(string) string { return "" }) {
		t.Fatal("empty env must stay color")
	}
	if !monoFor(func(k string) string {
		return map[string]string{"NO_COLOR": "1"}[k]
	}) {
		t.Fatal("NO_COLOR=1 must force mono")
	}
	if !monoFor(func(k string) string {
		return map[string]string{"NO_COLOR": "1", "COLORTERM": "truecolor"}[k]
	}) {
		t.Fatal("NO_COLOR must win over COLORTERM=truecolor")
	}
	if !monoFor(func(k string) string {
		return map[string]string{"TERM": "dumb"}[k]
	}) {
		t.Fatal("TERM=dumb must force mono")
	}
	if monoFor(func(k string) string {
		return map[string]string{"TERM": "xterm-256color"}[k]
	}) {
		t.Fatal("normal TERM must keep color")
	}
}
func TestMonoPaletteStripsColors(t *testing.T) {
	restore := SetMono(true)
	defer restore()
	chip := ChipFor("ok", "git")
	if strings.Contains(chip, "\x1b[3") || strings.Contains(chip, "\x1b[38;2;") {
		t.Fatalf("mono chip must carry no color SGR: %q", chip)
	}
	// Glyph + label survive (Reset sequences are fine — structure, not color).
	plainChip := strings.ReplaceAll(strings.ReplaceAll(chip, "\x1b[0m", ""), "\x1b[2m", "")
	if !strings.Contains(plainChip, "● git") {
		t.Fatalf("mono chip keeps glyph+label: %q", chip)
	}
}

// --- PAUSED aggregate + hero attention banner ------------------------------

func TestAggregateStatusPaused(t *testing.T) {
	snap := testSnapshot()
	st := *snap.Status
	st.Runs = &RunsPayload{Active: fptr(0)}
	st.Queue = &StatusQueueCounts{Pending: fptr(0), Claimed: fptr(0), Done: fptr(0)}
	st.Ask = &StatusAsk{ID: "TASK-ask"}
	if got := AggregateStatus(&st, nil); got != "PAUSED" {
		t.Fatalf("paused ask = %s, want PAUSED", got)
	}
	// RUNNING outranks PAUSED — live work stays the loudest state.
	st.Queue.Claimed = fptr(1)
	if got := AggregateStatus(&st, nil); got != "RUNNING" {
		t.Fatalf("running+paused = %s, want RUNNING", got)
	}
	st.Ask = nil
	st.Queue.Claimed = fptr(0)
	if got := AggregateStatus(&st, nil); got != "IDLE" {
		t.Fatalf("no ask = %s, want IDLE", got)
	}
}

func TestHeroPausedBannerShowsAnswerKey(t *testing.T) {
	snap := testSnapshot()
	// Quiet the board first: RUNNING outranks PAUSED by design, so the
	// banner contract is asserted from an otherwise-idle snapshot.
	snap.Agents.Panes[0].State = "idle"
	snap.Status.Runs = &RunsPayload{Active: fptr(0)}
	snap.Status.Queue = &StatusQueueCounts{Pending: fptr(0), Claimed: fptr(0), Done: fptr(0)}
	snap.Status.Ask = &StatusAsk{ID: "TASK-ask"}
	out := plain(RenderDashboard(snap, RenderOptions{}))
	if !strings.Contains(out, "paused") || !strings.Contains(out, "[g] answer") {
		t.Fatalf("paused task must surface the g answer cue:\n%s", out)
	}
	if !strings.Contains(out, "PAUSED") {
		t.Fatalf("header chip must show PAUSED:\n%s", out)
	}
}

// --- log search: filtered-space jump math ----------------------------------

func searchTestLoop(t *testing.T) *loop {
	t.Helper()
	l := newTestLoop(t, TuiOptions{})
	l.view = ViewLog
	for i := 0; i < 30; i++ {
		msg := "line " + itoa(i)
		if i == 5 || i == 15 || i == 25 {
			msg = "needle at " + itoa(i)
		}
		l.logLines = append(l.logLines, LogLine{Level: "info", Stage: "run", Message: msg})
	}
	l.logSearch = "needle"
	return l
}

func TestJumpLogMatchColdStartGoesNewest(t *testing.T) {
	l := searchTestLoop(t)
	l.jumpLogMatchLocked(1)
	if l.logMatchIdx != 2 {
		t.Fatalf("cold start pos = %d, want 2 (newest of 3 matches)", l.logMatchIdx)
	}
	if l.logScroll != 0 || !l.logFollow {
		t.Fatalf("newest match = follow mode, scroll=%d", l.logScroll)
	}
}

func TestJumpLogMatchNextWrapsToOldest(t *testing.T) {
	l := searchTestLoop(t)
	l.jumpLogMatchLocked(1)  // newest (pos 2)
	l.jumpLogMatchLocked(-1) // N steps to pos 1
	if l.logMatchIdx != 1 {
		t.Fatalf("N pos = %d, want 1", l.logMatchIdx)
	}
	// Viewport: 3 matches, pos 1 → scroll = 3-1-1 = 1 (match last visible row).
	if l.logScroll != 1 || l.logFollow {
		t.Fatalf("pos-1 scroll = %d, want 1", l.logScroll)
	}
	l.jumpLogMatchLocked(-1) // wraps to oldest (pos 0)
	if l.logMatchIdx != 0 || l.logScroll != 2 {
		t.Fatalf("wrap pos=%d scroll=%d, want 0/2", l.logMatchIdx, l.logScroll)
	}
	l.jumpLogMatchLocked(1)
	if l.logMatchIdx != 1 {
		t.Fatalf("n from oldest = %d, want 1", l.logMatchIdx)
	}
}

func TestLogSearchFiltersViewportToMatches(t *testing.T) {
	l := searchTestLoop(t)
	l.logScroll = 0
	l.logFollow = true
	out := plain(RenderDashboard(l.snap, RenderOptions{
		View: ViewLog,
		Log: &LogViewState{
			Lines: l.logLines, Scroll: 0, Follow: true,
			State: "live", Search: l.logSearch,
		},
		Rows: 40, Width: 100,
	}))
	if !strings.Contains(out, "needle at 25") {
		t.Fatalf("filtered tail must show the newest match:\n%s", out)
	}
	if strings.Contains(out, "line 29") {
		t.Fatal("non-matching lines must not render when a filter is active")
	}
	if !strings.Contains(out, "3 match(es)") {
		t.Fatal("title must count matches")
	}
}

func TestApplyKeysSearchPromptFlow(t *testing.T) {
	l := searchTestLoop(t)
	l.logSearch = ""
	// / opens the prompt; draft edits live.
	l.ApplyKeys(DecodeKeys("/", false))
	if !l.logSearchMode {
		t.Fatal("/ must open the search prompt")
	}
	l.ApplyKeys(DecodeKeys("needl", false))
	if l.logSearchDraft != "needl" {
		t.Fatalf("draft = %q, want needl", l.logSearchDraft)
	}
	// Backspace trims one rune.
	l.ApplyKeys(DecodeKeys("\x7f", false))
	if l.logSearchDraft != "need" {
		t.Fatalf("backspace draft = %q, want need", l.logSearchDraft)
	}
	// Finish the draft and commit: "le" completes "needle", then a flushed
	// Enter applies it and jumps to the newest match.
	l.ApplyKeys(DecodeKeys("le", false))
	l.ApplyKeys(DecodeKeys("\r", true))
	if l.logSearch != "needle" || l.logSearchMode {
		t.Fatalf("commit: search=%q mode=%v", l.logSearch, l.logSearchMode)
	}
	if l.logMatchIdx != 2 || l.logScroll != 0 || !l.logFollow {
		t.Fatalf("commit must jump to newest match, pos=%d scroll=%d", l.logMatchIdx, l.logScroll)
	}
}

func TestApplyKeysSearchPromptCommitAndCancel(t *testing.T) {
	l := searchTestLoop(t)
	l.logSearch = ""
	l.ApplyKeys(DecodeKeys("/", false))
	l.ApplyKeys(DecodeKeys("needle", false))
	// A burst carrying Enter then 'n': Enter commits, n then jumps matches.
	if quit := l.ApplyKeys(DecodeKeys("\rn", false)); quit {
		t.Fatal("search flow must not quit")
	}
	if l.logSearch != "needle" || l.logSearchMode {
		t.Fatalf("commit: search=%q mode=%v", l.logSearch, l.logSearchMode)
	}
	// Enter commits and jumps to the newest match (pos 2); the trailing 'n'
	// in the burst then walks to the next match — wrapping to the oldest
	// (pos 0, scroll = 3-1-0 = 2 in filtered space).
	if l.logMatchIdx != 0 || l.logScroll != 2 {
		t.Fatalf("after commit+n: pos=%d scroll=%d, want 0/2", l.logMatchIdx, l.logScroll)
	}
	// Esc clears the search (flushed — a lone ESC is a pending partial).
	l.ApplyKeys(DecodeKeys("\x1b", true))
	if l.logSearch != "" {
		t.Fatalf("Esc must clear the search, got %q", l.logSearch)
	}
}

func TestApplyKeysSearchEscCancelsPrompt(t *testing.T) {
	l := searchTestLoop(t)
	l.logSearch = ""
	l.ApplyKeys(DecodeKeys("/", false))
	l.ApplyKeys(DecodeKeys("zzz", false))
	l.ApplyKeys(DecodeKeys("\x1b", true))
	if l.logSearchMode {
		t.Fatal("Esc must close the prompt")
	}
	if l.logSearch != "" {
		t.Fatalf("cancelled prompt must not commit, got %q", l.logSearch)
	}
}

// --- selection-following viewports ------------------------------------------

func TestSessionsViewportFollowsSelection(t *testing.T) {
	snap := testSnapshot()
	for i := 0; i < 20; i++ {
		snap.Agents.Panes = append(snap.Agents.Panes, TuiPane{
			TaskID: "TASK-" + itoa(i), PaneID: "w1:p" + itoa(i),
			State: "idle", Cwd: "/tmp/r" + itoa(i),
		})
	}
	// 21 panes (base + 20). Short terminal: only a few rows fit; select the
	// LAST pane (index 20) and expect exactly it visible.
	out := plain(RenderDashboard(snap, RenderOptions{View: ViewSessions, Selection: 20, Rows: 12, Width: 100}))
	if !strings.Contains(out, "TASK-19") {
		t.Fatalf("selected pane must stay visible with a short terminal:\n%s", out)
	}
	if strings.Contains(out, "TASK-0") {
		t.Fatal("panes above the window must be cut")
	}
	if !strings.Contains(out, "hidden") {
		t.Fatal("the cut must be announced on the title line")
	}
}

func TestWorkersViewportFollowsSelection(t *testing.T) {
	snap := testSnapshot()
	for i := 0; i < 12; i++ {
		snap.Agents.Panes = append(snap.Agents.Panes, TuiPane{
			TaskID: "TASK-" + itoa(i), PaneID: "w1:p" + itoa(i),
			State: "idle", Cwd: "/tmp/r" + itoa(i),
		})
	}
	// 13 panes + 1 queued = 14 cards; select the LAST card (the queued one).
	last := 13
	out := plain(RenderDashboard(snap, RenderOptions{View: ViewWorkers, Selection: last, Rows: 14, Width: 100}))
	// The selected card must stay visible even when the budget fits one.
	if !strings.Contains(out, "▸ TASK-xyz") {
		t.Fatalf("selected card must stay visible:\n%s", out)
	}
	if !strings.Contains(out, "hidden") {
		t.Fatal("hidden cards must be counted on the Workers title")
	}
	// Mid-roster selection: the window keeps TASK-6 on screen with the cut
	// announced on both sides.
	out = plain(RenderDashboard(snap, RenderOptions{View: ViewWorkers, Selection: 7, Rows: 14, Width: 100}))
	if !strings.Contains(out, "TASK-6") {
		t.Fatalf("mid-roster selection must stay visible:\n%s", out)
	}
}

// --- terminal title (OSC 2) --------------------------------------------------

func TestAggregateTitleLocked(t *testing.T) {
	l := newTestLoop(t, TuiOptions{})
	l.snap = testSnapshot()
	if got := l.aggregateTitleLocked(); got != "RUNNING" {
		t.Fatalf("title aggregate = %q, want RUNNING", got)
	}
	l.snap = &Snapshot{}
	if got := l.aggregateTitleLocked(); got != "offline" {
		t.Fatalf("offline title = %q, want offline", got)
	}
	l.snap = testSnapshot()
	l.snap.Status = nil
	if got := l.aggregateTitleLocked(); got != "offline" {
		t.Fatalf("nil status title = %q, want offline", got)
	}
}
