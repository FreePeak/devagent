package tui

import (
	"encoding/json"
	"strings"
	"testing"
)

// Port of test/status-card.test.ts (FR-SIMPLE-03/04): the status card
// composes phase + one next action in the §20.8 card/chip language. The
// board/queue/panes arrive as plain data (sibling-isolation seam).

func statusTask(id, status string) StatusTask {
	return StatusTask{ID: id, Title: "Task " + id, Status: status}
}

func statusBoard(tasks []StatusTask) *StatusBoard {
	return &StatusBoard{Goal: "Add CSV export", Tasks: tasks}
}

func statusPane(taskID, state string) StatusPane {
	return StatusPane{TaskID: taskID, State: state}
}

func TestStatusViewNotStarted(t *testing.T) {
	v := ComposeStatusView(nil, QueueCounts{}, nil, false)
	if v.Phase != "not started" || !strings.Contains(v.NextAction, "devagent init") || v.BoardExists {
		t.Fatalf("not-started view = %+v", v)
	}
	if v.Detail != "no setup yet" {
		t.Fatalf("detail = %q", v.Detail)
	}
}

func TestStatusViewConfiguredNoBoard(t *testing.T) {
	v := ComposeStatusView(nil, QueueCounts{}, nil, true)
	if v.Phase != "not started" || !strings.Contains(v.NextAction, "devagent orchestrate --goal") {
		t.Fatalf("configured view = %+v", v)
	}
	if v.Detail != "config found — no goal dispatched yet" {
		t.Fatalf("detail = %q", v.Detail)
	}
}

func TestStatusViewRunningWithPane(t *testing.T) {
	v := ComposeStatusView(statusBoard([]StatusTask{statusTask("T1", "dispatched")}),
		QueueCounts{}, []StatusPane{statusPane("T1", "running")}, true)
	if v.Phase != "implementing" || v.ChipState != "running" {
		t.Fatalf("phase = %q chip = %q", v.Phase, v.ChipState)
	}
	if v.AttachHint != "devagent attach T1" {
		t.Fatalf("attach hint = %q", v.AttachHint)
	}
	if v.CurrentTask == nil || v.CurrentTask.ID != "T1" {
		t.Fatalf("current task = %+v", v.CurrentTask)
	}
	if v.TaskCounts["dispatched"] != 1 {
		t.Fatalf("task counts = %v", v.TaskCounts)
	}
}

func TestStatusViewRunningNoPane(t *testing.T) {
	v := ComposeStatusView(statusBoard([]StatusTask{statusTask("T1", "dispatched")}),
		QueueCounts{}, nil, true)
	if v.AttachHint != "" {
		t.Fatalf("no pane must clear the attach hint: %q", v.AttachHint)
	}
	if !strings.Contains(v.NextAction, "devagent project") {
		t.Fatalf("next action = %q", v.NextAction)
	}
}

func TestStatusViewAskAndFailed(t *testing.T) {
	v := ComposeStatusView(statusBoard([]StatusTask{statusTask("T1", "ask"), {ID: "T2", Title: "Task T2", Status: "failed", FailureDetail: "tests red"}}),
		QueueCounts{}, nil, true)
	if v.Phase != "paused for you" || !strings.Contains(v.NextAction, "--answer T1=") {
		t.Fatalf("ask view = %+v", v)
	}
	if v.CurrentTask == nil || v.CurrentTask.ID != "T1" {
		t.Fatalf("ask current task = %+v", v.CurrentTask)
	}
	failed := ComposeStatusView(statusBoard([]StatusTask{{ID: "T2", Title: "Task T2", Status: "failed", FailureDetail: "tests red"}}),
		QueueCounts{}, nil, true)
	if failed.Phase != "failed" || failed.ChipState != "failed" ||
		!strings.Contains(failed.NextAction, "devagent project") {
		t.Fatalf("failed view = %+v", failed)
	}
	if !strings.Contains(failed.Detail, "tests red") {
		t.Fatalf("failed detail = %q", failed.Detail)
	}
}

func TestStatusViewUntrustedAndAllDone(t *testing.T) {
	// Untrusted task with no live pane: awaiting audit + board hint
	// (boardNextAction semantics).
	v := ComposeStatusView(statusBoard([]StatusTask{statusTask("T1", "untrusted")}),
		QueueCounts{}, nil, true)
	if v.Phase != "awaiting audit" {
		t.Fatalf("untrusted phase = %q", v.Phase)
	}
	if !strings.Contains(v.NextAction, "devagent project") {
		t.Fatalf("untrusted next action = %q", v.NextAction)
	}
	done := ComposeStatusView(statusBoard([]StatusTask{statusTask("T1", "done")}),
		QueueCounts{}, nil, true)
	if done.Phase != "all done" || done.ChipState != "ok" ||
		!strings.Contains(done.NextAction, "state a new goal") {
		t.Fatalf("all-done view = %+v", done)
	}
	if !strings.Contains(done.Detail, "goal: Add CSV export") {
		t.Fatalf("all-done detail = %q", done.Detail)
	}
}

func TestStatusViewQueuedWithoutBoard(t *testing.T) {
	v := ComposeStatusView(nil, QueueCounts{Total: 1, Pending: 1}, nil, false)
	if v.Phase != "queued" || v.Queue.Pending != 1 ||
		!strings.Contains(v.NextAction, "workers claim queued tasks automatically") {
		t.Fatalf("queued view = %+v", v)
	}
	if v.Detail != "1 task(s) waiting in the queue" {
		t.Fatalf("detail = %q", v.Detail)
	}
}

func TestRenderStatusCardLanguage(t *testing.T) {
	v := ComposeStatusView(statusBoard([]StatusTask{statusTask("T1", "dispatched")}),
		QueueCounts{}, []StatusPane{statusPane("T1", "running")}, true)
	card := plain(RenderStatusCard(v, 100))
	for _, want := range []string{"Project status ─", "●", "implementing", "next:", "devagent attach T1", "╰"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card missing %q:\n%s", want, card)
		}
	}
}

func TestStatusJSONParity(t *testing.T) {
	v := ComposeStatusView(statusBoard([]StatusTask{statusTask("T1", "dispatched")}),
		QueueCounts{Total: 1}, []StatusPane{statusPane("T1", "running")}, true)
	raw := StatusJSON(v)
	if strings.Contains(raw, "\x1b[") {
		t.Fatal("json must carry no ANSI codes")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("json invalid: %v", err)
	}
	if parsed["phase"] != "implementing" || parsed["chipState"] != "running" ||
		parsed["attachHint"] != "devagent attach T1" || parsed["boardExists"] != true ||
		parsed["goal"] != "Add CSV export" {
		t.Fatalf("json = %s", raw)
	}
	counts, ok := parsed["taskCounts"].(map[string]any)
	if !ok || counts["dispatched"] != float64(1) {
		t.Fatalf("taskCounts = %v", parsed["taskCounts"])
	}
	// Key order matches the TS object literal.
	keys := []string{"phase", "chipState", "goal", "boardExists", "currentTask", "taskCounts", "queue", "nextAction", "attachHint"}
	idx := 0
	for _, k := range keys {
		i := strings.Index(raw, `"`+k+`"`)
		if i < idx {
			t.Fatalf("key %s out of order in:\n%s", k, raw)
		}
		idx = i
	}
}
