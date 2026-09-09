package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// render assertions (no daemon — the transport/loop halves are seams).

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// plain strips ANSI escapes so chip/box contiguity can be asserted on
// visible text.
func plain(s string) string { return ansiRe.ReplaceAllString(s, "") }

func fptr(v float64) *float64 { return &v }

func testSnapshot() *Snapshot {
	return &Snapshot{
		Status: &StatusPayload{
			Now:     time.Now().UTC().Format(time.RFC3339Nano),
			UptimeS: fptr(5),
			Runs:    &RunsPayload{Active: fptr(1), FailedRecent: fptr(0)},
			Queue: &StatusQueueCounts{
				Pending: fptr(2), Claimed: fptr(1), Done: fptr(3),
			},
			Circuit: "closed",
			Herdr:   &HerdrStatus{Enabled: boolPtr(true), Session: "devagent"},
			Spawn:   &SpawnStatus{Visibility: "visible"},
			Capabilities: []string{
				"approve", "dispatch", "attach", "kill-via-answer",
			},
		},
		Agents: &AgentPayload{
			Panes: []TuiPane{{
				TaskID:      "TASK-abc",
				Role:        "worker",
				Worker:      "omp",
				PaneID:      "w1:p1",
				WorkspaceID: "w1",
				Label:       "TASK-abc-a1",
				Cwd:         "/tmp/.devagent-worktrees/TASK-abc-a1",
				AgentStatus: "working",
				State:       "running",
				StartedAt:   time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339Nano),
			}},
			Queued: []TuiQueuedTask{{
				ID:        "TASK-xyz",
				Title:     "do the thing",
				Status:    "pending",
				CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			}},
		},
		History: []HistoryRow{
			{"ts": time.Now().UTC().Format(time.RFC3339Nano), "kind": "audit", "taskId": "TASK-abc", "attempt": float64(1), "verdict": "pass", "integrity": "ok", "unmetCriteria": []any{}, "summary": ""},
			{"ts": time.Now().UTC().Format(time.RFC3339Nano), "kind": "event", "event": "watchdog-health", "taskId": "TASK-mtnmnp1g-f4j9", "attempt": float64(1), "worker": "omp", "site": "herdr-pane", "watchdogFired": true},
			{"ts": time.Now().UTC().Format(time.RFC3339Nano), "kind": "event", "event": "loop-result", "loop": float64(76), "status": "skipped", "goal": "Goal: " + strings.Repeat("a", 100)},
		},
		Sessions:  nil,
		Reachable: true,
		FetchedAt: time.Now(),
	}
}

func boolPtr(v bool) *bool { return &v }

func TestRenderDashboardFullFrame(t *testing.T) {
	snap := testSnapshot()
	out := plain(RenderDashboard(snap, RenderOptions{}))
	for _, want := range []string{
		"RUNNING", "2p/1c/3d", "herdr:devagent", "TASK-abc", "audit",
		"watchdog-health", "TASK-mtnmnp1g-f4j9", "loop:76", "fired", "skipped",
		"● working", "╭─", "● queued",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("frame missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "DAEMON UNREACHABLE") {
		t.Fatal("healthy snapshot must not show UNREACHABLE")
	}
	// goal prose capped, no full-width dump
	var goalRow string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "loop:76") {
			goalRow = l
			break
		}
	}
	if goalRow == "" || len([]rune(goalRow)) > 140 {
		t.Fatalf("goal row too long: %d", len([]rune(goalRow)))
	}
}

func TestRenderDashboardFooterAndHelp(t *testing.T) {
	snap := testSnapshot()
	out := plain(RenderDashboard(snap, RenderOptions{}))
	if !strings.Contains(out, "a attach") {
		t.Fatal("footer must advertise a attach")
	}
	help := plain(RenderDashboard(snap, RenderOptions{ShowHelp: true}))
	for _, want := range []string{
		"a         attach inline (FR-TUI-06)", "q or Ctrl+C  quit",
		"k  kill the running task", "y  confirm the pending kill",
		"switch view: workers / sessions / live log", "upgrade hint",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q", want)
		}
	}
}

func TestRenderDashboardFitLines(t *testing.T) {
	snap := testSnapshot()
	for _, rows := range []int{10, 12, 14, 20, 40} {
		out := RenderDashboard(snap, RenderOptions{Rows: rows, ShowHelp: true})
		if got := len(strings.Split(out, "\n")); got > rows {
			t.Fatalf("rows=%d: frame has %d lines", rows, got)
		}
	}
}

func TestAggregateStatusLiveOutranksFailedCount(t *testing.T) {
	snap := testSnapshot()
	st := *snap.Status
	runs := &RunsPayload{Active: fptr(1), FailedRecent: fptr(1)}
	st.Runs = runs
	if got := AggregateStatus(&st, nil); got != "RUNNING" {
		t.Fatalf("active runs = %s", got)
	}
	st2 := *snap.Status
	st2.Runs = &RunsPayload{Active: fptr(0), FailedRecent: fptr(1)}
	st2.Queue = &StatusQueueCounts{Pending: fptr(0), Claimed: fptr(2), Done: fptr(3)}
	if got := AggregateStatus(&st2, nil); got != "RUNNING" {
		t.Fatalf("claimed tasks = %s", got)
	}
	st3 := *snap.Status
	st3.Runs = &RunsPayload{Active: fptr(0), FailedRecent: fptr(1)}
	st3.Queue = &StatusQueueCounts{Pending: fptr(0), Claimed: fptr(0), Done: fptr(3)}
	st3.Circuit = "closed"
	if got := AggregateStatus(&st3, nil); got != "IDLE" {
		t.Fatalf("lifetime failed count must not pin FAILED: %s", got)
	}
	st4 := *snap.Status
	st4.Runs = &RunsPayload{Active: fptr(0), FailedRecent: fptr(0)}
	st4.Queue = &StatusQueueCounts{Pending: fptr(0), Claimed: fptr(0), Done: fptr(3)}
	st4.Circuit = "open"
	if got := AggregateStatus(&st4, nil); got != "FAILED" {
		t.Fatalf("open circuit = %s", got)
	}
	if got := AggregateStatus(nil, nil); got != "IDLE" {
		t.Fatalf("nil status = %s", got)
	}
}

func TestRenderDashboardIterationPhase(t *testing.T) {
	snap := testSnapshot()
	snap.History = append(snap.History, HistoryRow{
		"ts": time.Now().UTC().Format(time.RFC3339Nano), "kind": "event",
		"event": "loop-phase", "loop": float64(82), "phase": "task",
		"detail": "Ship the Q27 cross-board retry-memory",
	})
	out := plain(RenderDashboard(snap, RenderOptions{}))
	for _, want := range []string{"iteration 82", "phase: task", "Ship the Q27 cross-board retry-memory"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q", want)
		}
	}
	plainOut := plain(RenderDashboard(testSnapshot(), RenderOptions{}))
	if strings.Contains(plainOut, "phase:") {
		t.Fatal("no loop-phase rows must yield no iteration line")
	}
}

func TestRenderDashboardUnreachable(t *testing.T) {
	snap := testSnapshot()
	snap.Status = nil
	snap.Reachable = false
	if out := RenderDashboard(snap, RenderOptions{}); !strings.Contains(out, "DAEMON UNREACHABLE") {
		t.Fatal("dead snapshot must degrade to UNREACHABLE")
	}
}

func TestRenderDashboardSessionsView(t *testing.T) {
	snap := testSnapshot()
	out := plain(RenderDashboard(snap, RenderOptions{ShowSessions: true}))
	for _, want := range []string{"Sessions", "w1:p1", ".devagent-worktrees"} {
		if !strings.Contains(out, want) {
			t.Fatalf("sessions view missing %q", want)
		}
	}
}

func TestRenderDashboardMetricsLine(t *testing.T) {
	snap := testSnapshot()
	out := plain(RenderDashboard(snap, RenderOptions{
		Metrics: &MetricsState{Samples: []float64{0, 2, 1, 3}, SampleMs: 2000},
	}))
	for _, want := range []string{"2p/1c/3d", "queue [", "up 5s", "herdr:devagent"} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics line missing %q", want)
		}
	}
	if !regexp.MustCompile(`activity\(8s\) [▁▂▃▄▅▆▇█]+ 3`).MatchString(out) {
		t.Fatalf("sparkline missing: %s", out)
	}
	bare := plain(RenderDashboard(testSnapshot(), RenderOptions{}))
	if strings.Contains(bare, "activity(") {
		t.Fatal("without samples the sparkline must be absent, never fabricated")
	}
}

func TestRenderDashboardLogView(t *testing.T) {
	snap := testSnapshot()
	lines := []LogLine{
		ParseLogLine(`{"ts":"2026-09-05T10:00:00.000Z","level":"warn","stage":"clarify","runId":"run-1234","message":"Insufficient specification"}`),
		ParseLogLine("plain corruption"),
	}
	out := plain(RenderDashboard(snap, RenderOptions{
		View: ViewLog,
		Log:  &LogViewState{Lines: lines, Scroll: 0, Follow: true, State: "live", Source: "run-1234"},
	}))
	for _, want := range []string{
		"▌Live log", "● live", "line(s) buffered", "run run-123",
		"[following tail]", "warn", "clarify", "Insufficient specification",
		"plain corruption",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log view missing %q", want)
		}
	}
}

func TestRenderDashboardLogScrolled(t *testing.T) {
	snap := testSnapshot()
	many := make([]LogLine, 40)
	base := time.Now()
	for i := range many {
		many[i] = ParseLogLine(`{"ts":"` + base.Add(-time.Duration(i)*time.Second).UTC().Format(time.RFC3339Nano) + `","level":"info","stage":"impl","message":"event ` + itoa(i) + `"}`)
	}
	out := RenderDashboard(snap, RenderOptions{
		View: ViewLog,
		Log:  &LogViewState{Lines: many, Scroll: 10, Follow: false, State: "live"},
		Rows: 24,
	})
	plainOut := plain(out)
	if !strings.Contains(plainOut, "10 older") {
		t.Fatal("scroll offset missing")
	}
	if !strings.Contains(plainOut, "event 18") {
		t.Fatal("viewport must start 10+viewport lines back")
	}
	if strings.Contains(plainOut, "event 0") {
		t.Fatal("newest must be hidden while scrolled back")
	}
	if !strings.Contains(plainOut, "Live log") {
		t.Fatal("title is never cut by fitting")
	}
	if got := len(strings.Split(out, "\n")); got > 24 {
		t.Fatalf("frame has %d lines, htop always fits", got)
	}
}

func TestRenderDashboardOverlays(t *testing.T) {
	snap := testSnapshot()
	detail := plain(RenderDashboard(snap, RenderOptions{
		Overlay: DetailOverlay(&snap.Agents.Panes[0], nil),
	}))
	for _, want := range []string{"TASK-abc", "pane w1:p1", "workspace w1", "devagent attach TASK-abc"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail overlay missing %q", want)
		}
	}
	up := plain(RenderDashboard(snap, RenderOptions{Overlay: UpgradeOverlay()}))
	for _, want := range []string{"Upgrade", "git pull --ff-only", "make build", "rollback"} {
		if !strings.Contains(up, want) {
			t.Fatalf("upgrade overlay missing %q", want)
		}
	}
}

func TestRenderDashboardSelection(t *testing.T) {
	snap := testSnapshot()
	sel := plain(RenderDashboard(snap, RenderOptions{Selection: 1})) // 0 = pane, 1 = queued
	if !strings.Contains(sel, "▸ TASK-xyz") {
		t.Fatal("queued card must carry the selection cursor")
	}
	if strings.Contains(sel, "▸ TASK-abc") {
		t.Fatal("unselected pane must not carry the cursor")
	}
	small := RenderLines(snap, RenderOptions{Rows: 12})
	if len(small) > 12 {
		t.Fatalf("small terminal frame has %d lines", len(small))
	}
	if joined := strings.Join(small, "\n"); !strings.Contains(plain(joined), "DevAgent") {
		t.Fatal("header survives the trim")
	}
}

func TestRenderDashboardPoisonedSessionsShape(t *testing.T) {
	// Hand-built snapshot with the poisoned /sessions shape: must degrade to
	// a rendered pane list, never crash (the 2026-09-05 sessions crash).
	snap := &Snapshot{
		Status:    nil,
		Agents:    nil,
		History:   nil,
		Sessions:  []TuiPane{testSnapshot().Agents.Panes[0]},
		Reachable: true,
	}
	out := plain(RenderDashboard(snap, RenderOptions{View: ViewSessions}))
	if !strings.Contains(out, "Sessions") || !strings.Contains(out, "w1:p1") {
		t.Fatalf("poisoned shape must render from the fallback roster:\n%s", out)
	}
	if strings.Contains(out, "no live sessions") {
		t.Fatal("fallback roster must win")
	}
}

func TestRenderDashboardFixture(t *testing.T) {
	// Fixture-driven render: the pinned snapshot.json (the same state shape
	// test/tui.test.ts builds inline) must render the same visible board.
	SetNow(func() time.Time { return time.Date(2026, 9, 7, 9, 5, 0, 0, time.UTC) })
	defer SetNow(nil)
	raw, err := os.ReadFile(filepath.Join("testdata", "dashboard", "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("fixture unparseable: %v", err)
	}
	out := plain(RenderDashboard(&snap, RenderOptions{}))
	for _, want := range []string{"RUNNING", "2p/1c/3d", "herdr:devagent", "TASK-abc", "audit",
		"watchdog-health", "loop:76", "skipped", "● working", "● queued",
		// goalW caps at min(60, max(30, width-66)) = 34 columns at width 100
		"Goal: pinned by the FR-GO-11 fixt…"} {
		if !strings.Contains(out, want) {
			t.Fatalf("fixture render missing %q", want)
		}
	}
}
func TestPickKillTarget(t *testing.T) {
	snap := testSnapshot()
	if got := PickKillTarget(snap, ViewWorkers, 0); got != "TASK-abc" {
		t.Fatalf("selection kill target = %q", got)
	}
	if got := PickKillTarget(snap, ViewWorkers, 1); got != "TASK-xyz" {
		t.Fatalf("queued kill target = %q", got)
	}
	empty := &Snapshot{}
	if got := PickKillTarget(empty, ViewWorkers, -1); got != "" {
		t.Fatalf("empty snapshot target = %q", got)
	}
}

func TestFormatJSNumberParity(t *testing.T) {
	if jsNum(2) != "2" || jsNum(0) != "0" || jsNum(76) != "76" {
		t.Fatal("integral floats render without decimal point")
	}
}

func TestNormalizeAgentsEnvelope(t *testing.T) {
	if NormalizeAgents(nil) != nil {
		t.Fatal("non-object payload must be nil")
	}
	v := map[string]any{
		"panes":  []any{"not-an-object"},
		"queued": []any{},
	}
	ag := NormalizeAgents(v)
	if ag == nil || len(ag.Panes) != 0 || len(ag.Queued) != 0 {
		t.Fatalf("non-object pane rows must degrade to empty: %+v", ag)
	}
}
