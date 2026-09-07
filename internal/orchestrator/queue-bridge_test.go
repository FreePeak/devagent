// Package file mirrors test/queue-bridge.test.ts (FR-GO-07, issue #194).
package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/queue"
)

// qbEnqueue mirrors enqueueTask(repo, {id,title,goal,acceptanceCriteria}).
func qbEnqueue(t *testing.T, repo, id, title, goal string, ac []string) *queue.QueuedTask {
	t.Helper()
	q, err := queue.EnqueueTask(repo, queue.EnqueueInput{ID: id, Title: title, Goal: goal, AcceptanceCriteria: ac})
	if err != nil {
		t.Fatalf("EnqueueTask(%s): %v", id, err)
	}
	return q
}

// qbPlannerTask mirrors the planner stub task shape.
func qbPlannerTask(id, title, prompt string, dependsOn []string) OrchestratorTask {
	return OrchestratorTask{ID: id, Title: title, Prompt: prompt, DependsOn: dependsOn, Status: TaskStatusPending, Attempts: 0}
}

// qbBoardJSON parses the repo's board file.
func qbBoardJSON(t *testing.T, repo string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, BoardFile))
	if err != nil {
		t.Fatal(err)
	}
	var board map[string]any
	if err := json.Unmarshal(data, &board); err != nil {
		t.Fatal(err)
	}
	return board
}

func TestQueueBridge(t *testing.T) {
	t.Run("bridges a queued goal into a board via planner (idempotent)", func(t *testing.T) {
		repo := t.TempDir()
		q := qbEnqueue(t, repo, "SCOUT-2026-01-01-aaaa", "Add audit trail", "Goal: add audit table", []string{"migration applies"})
		planner := func(string) []OrchestratorTask {
			return []OrchestratorTask{
				qbPlannerTask("T1", "Draft migration", "create migration", []string{}),
				qbPlannerTask("T2", "Add API", "add endpoint", []string{"T1"}),
			}
		}
		r := BridgeQueueToBoard(q, BridgeOptions{RepoPath: repo, Planner: planner})
		if !r.Created {
			t.Errorf("created = false, want true (%+v)", r)
		}
		if _, err := os.Stat(filepath.Join(repo, BoardFile)); err != nil {
			t.Fatal("board file must exist after bridge")
		}
		board := qbBoardJSON(t, repo)
		tasks, _ := board["tasks"].([]any)
		if len(tasks) != 2 {
			t.Errorf("board tasks = %d, want 2", len(tasks))
		}
		// source queue item retired so the builder lane never double-builds it
		retired := queue.ReadTask(repo, q.ID)
		if retired == nil || retired.Status != queue.StatusDone {
			t.Errorf("queue item status = %+v, want done", retired)
		}
		// idemp: second call returns existing board without wiping tasks
		r2 := BridgeQueueToBoard(q, BridgeOptions{RepoPath: repo, Planner: planner})
		if !r2.Idempotent || r2.TasksWritten != 2 {
			t.Errorf("second call = %+v, want idempotent/2", r2)
		}
	})

	t.Run("falls back to single-task plan when planner fails", func(t *testing.T) {
		repo := t.TempDir()
		q := qbEnqueue(t, repo, "G-1", "x", "Goal: do a tiny edit", nil)
		r := BridgeQueueToBoard(q, BridgeOptions{RepoPath: repo, Planner: func(string) []OrchestratorTask { return nil }})
		if r.TasksWritten != 1 {
			t.Errorf("tasksWritten = %d, want 1 (fallback plan)", r.TasksWritten)
		}
	})

	t.Run("carries the prior archived board failure class onto a re-bridged goal (Q27)", func(t *testing.T) {
		repo := t.TempDir()
		q := qbEnqueue(t, repo, "REBRIDGE-1", "Add audit trail", "Goal: add audit table", []string{"migration applies"})
		goal := "Goal: add audit table\nAcceptance criteria:\n- migration applies"
		archiveDir := filepath.Join(repo, ".devagent", "archive")
		if err := os.MkdirAll(archiveDir, 0o755); err != nil {
			t.Fatal(err)
		}
		archived := map[string]any{
			"goal":      goal,
			"createdAt": "2026-09-03T04:00:00.000Z",
			"updatedAt": "2026-09-03T05:00:00.000Z",
			"roles":     map[string]any{"planner": "omp", "executor": "omp"},
			"tasks": []map[string]any{{
				"id": "T1", "title": "Draft migration", "prompt": "create migration",
				"dependsOn": []any{}, "status": "failed", "attempts": 3,
				"interrupt": map[string]any{"failureClass": "test-gate", "lastGateExcerpt": "1 failed", "attempts": 3, "trailHash": "abc123"},
			}},
		}
		data, err := json.Marshal(archived)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(archiveDir, "board-stuck-20260903-120000.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		planner := func(string) []OrchestratorTask {
			return []OrchestratorTask{qbPlannerTask("T1", "Draft migration", "create migration", []string{})}
		}
		r := BridgeQueueToBoard(q, BridgeOptions{RepoPath: repo, Planner: planner})
		if !r.Created {
			t.Errorf("created = false, want true (%+v)", r)
		}
		board := qbBoardJSON(t, repo)
		// Re-bridged goal carries the prior board's failure class instead of a
		// fresh attempt budget with no memory of why the last board died.
		if board["failureClass"] != "test-gate" {
			t.Errorf("failureClass = %v, want test-gate", board["failureClass"])
		}
	})

	t.Run("leaves failureClass unset when no archived board matches the goal", func(t *testing.T) {
		repo := t.TempDir()
		q := qbEnqueue(t, repo, "FRESH-1", "x", "Goal: brand new work", nil)
		r := BridgeQueueToBoard(q, BridgeOptions{RepoPath: repo, Planner: func(string) []OrchestratorTask { return nil }})
		if !r.Created {
			t.Errorf("created = false, want true (%+v)", r)
		}
		board := qbBoardJSON(t, repo)
		if _, ok := board["failureClass"]; ok {
			t.Errorf("failureClass = %v, want absent", board["failureClass"])
		}
	})

	t.Run("bridgeIfQueued picks oldest pending and is no-op when no pending", func(t *testing.T) {
		repo := t.TempDir()
		if r := BridgeIfQueued(repo, nil); r != nil {
			t.Errorf("empty queue = %+v, want nil", r)
		}
		qbEnqueue(t, repo, "A-1", "first", "Goal: first", nil)
		qbEnqueue(t, repo, "B-1", "second", "Goal: second", nil)
		r := BridgeIfQueued(repo, func(g string) []OrchestratorTask {
			title := g
			if len(title) > 30 {
				title = title[:30]
			}
			return []OrchestratorTask{qbPlannerTask("T1", title, g, []string{})}
		})
		if r == nil || r.TasksWritten != 1 {
			t.Errorf("first bridge = %+v, want 1 task", r)
		}
		// board exists now, second bridgeIfQueued returns idempotent board, not a new goal
		r2 := BridgeIfQueued(repo, nil)
		if r2 == nil || !r2.Idempotent {
			t.Errorf("second bridge = %+v, want idempotent", r2)
		}
	})

	// queuedGoalString acceptance-criteria composition (the composed goal the
	// archived board matching keys on).
	t.Run("queuedGoalString appends acceptance criteria as a dash list", func(t *testing.T) {
		q := &queue.QueuedTask{Goal: "Goal: add audit table", AcceptanceCriteria: []string{"migration applies", "idempotent"}}
		want := "Goal: add audit table\nAcceptance criteria:\n- migration applies\n- idempotent"
		if got := QueuedGoalString(q); got != want {
			t.Errorf("queuedGoalString = %q, want %q", got, want)
		}
		if got := QueuedGoalString(&queue.QueuedTask{Goal: "Goal: plain"}); got != "Goal: plain" {
			t.Errorf("no-AC goal = %q", got)
		}
	})

	// archivedBoardFailureClass edge cases beyond the bridge test.
	t.Run("archivedBoardFailureClass scans newest first and tolerates corrupt entries", func(t *testing.T) {
		repo := t.TempDir()
		if got := ArchivedBoardFailureClass(repo, "Goal: x"); got != "" {
			t.Errorf("missing archive dir = %q, want empty", got)
		}
		dir := filepath.Join(repo, ".devagent", "archive")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "board-stuck-20260901-000000.json"), []byte("{corrupt"), 0o644); err != nil {
			t.Fatal(err)
		}
		older := map[string]any{"goal": "Goal: same goal", "failureClass": "older-class", "tasks": []any{}}
		writeQBJSON(t, filepath.Join(dir, "board-stuck-20260902-000000.json"), older)
		newer := map[string]any{"goal": "Goal: same goal", "failureClass": "newer-class", "tasks": []any{}}
		writeQBJSON(t, filepath.Join(dir, "board-stuck-20260905-000000.json"), newer)
		if got := ArchivedBoardFailureClass(repo, "Goal: same goal"); got != "newer-class" {
			t.Errorf("class = %q, want newer-class (newest first)", got)
		}
		if got := ArchivedBoardFailureClass(repo, "Goal: entirely different (Q1)"); got != "" {
			t.Errorf("unmatched goal = %q, want empty", got)
		}
		// goal prefix fallback (60-char cap) matches too
		longGoal := "Goal: " + strings.Repeat("y", 70)
		pfx := map[string]any{"goal": longGoal, "tasks": []any{}}
		writeQBJSON(t, filepath.Join(dir, "board-20260906-000000.json"), pfx)
		// goal prefix fallback (60-char cap) matches; this board carries no
		// class and no interrupted task, so the call returns empty even though
		// a different goal matched earlier.
		if got := ArchivedBoardFailureClass(repo, "Goal: "+strings.Repeat("y", 90)); got != "" {
			t.Errorf("prefix-match = %q, want empty (no class anywhere on match path)", got)
		}
	})
}

func writeQBJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
