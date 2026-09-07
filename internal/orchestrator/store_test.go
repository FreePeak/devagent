// Package file mirrors test/orchestrator-store.test.ts (FR-GO-07, issue #194).
package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stTempRepo(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "da-orch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// stTask builds the plain TS task fixture {id,title,prompt,dependsOn,status,attempts}.
func stTask(id, title, prompt string, dependsOn []string, status TaskStatus, attempts int) OrchestratorTask {
	return OrchestratorTask{
		ID:        id,
		Title:     title,
		Prompt:    prompt,
		DependsOn: dependsOn,
		Status:    status,
		Attempts:  attempts,
	}
}

func TestBoardStore(t *testing.T) {
	t.Run("saves and loads round-trip with readiness recomputed", func(t *testing.T) {
		repo := stTempRepo(t)
		SaveBoard(repo, CreateBoard("goal", []OrchestratorTask{
			stTask("T1", "a", "p", []string{}, TaskStatusDone, 1),
			stTask("T2", "b", "q", []string{"T1"}, TaskStatusPending, 0),
		}, ProjectBoardRoles{Planner: "claude-code", Executor: "opencode"}))
		loaded := LoadBoard(repo)
		if loaded == nil {
			t.Fatal("LoadBoard returned nil")
		}
		if loaded.Goal != "goal" {
			t.Errorf("goal = %q, want %q", loaded.Goal, "goal")
		}
		// T2 was pending with done dep -> saved as ready
		if got := loaded.Tasks[1].Status; got != TaskStatusReady {
			t.Errorf("T2 status = %q, want %q", got, TaskStatusReady)
		}
	})

	t.Run("returns nil for missing or corrupted boards", func(t *testing.T) {
		if got := LoadBoard(stTempRepo(t)); got != nil {
			t.Errorf("missing board: got %v, want nil", got)
		}
		repo := stTempRepo(t)
		if err := os.WriteFile(filepath.Join(repo, BoardFile), []byte("{corrupt"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := LoadBoard(repo); got != nil {
			t.Errorf("corrupt board: got %v, want nil", got)
		}
	})

	t.Run("writes atomically: no .tmp file left behind", func(t *testing.T) {
		repo := stTempRepo(t)
		SaveBoard(repo, CreateBoard("g", nil, ProjectBoardRoles{Planner: "claude-code", Executor: "opencode"}))
		raw, err := os.ReadFile(filepath.Join(repo, BoardFile))
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Goal string `json:"goal"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed.Goal != "g" {
			t.Errorf("goal = %q, want %q", parsed.Goal, "g")
		}
		if _, err := os.Stat(filepath.Join(repo, BoardFile+".tmp")); !os.IsNotExist(err) {
			t.Errorf(".tmp file should not exist (stat err = %v)", err)
		}
		// byte parity: 2-space indent + trailing newline
		if !strings.HasSuffix(string(raw), "\n") {
			t.Error("board file must end with a newline")
		}
		if !strings.Contains(string(raw), "\n  \"createdAt\"") {
			t.Error("board file must be 2-space indented")
		}
	})
}

func TestFormatPlanOnly(t *testing.T) {
	t.Run("renders full contracts with criteria and constraints for review", func(t *testing.T) {
		board := CreateBoard("ship export button", []OrchestratorTask{
			{
				ID:                  "T1",
				Title:               "schema migration",
				Prompt:              "add the exports table",
				AcceptanceCriteria:  []string{"migrations/001_exports.up.sql exists", "down migration pairs"},
				BoundaryConstraints: []string{"do not touch auth schema"},
				DependsOn:           []string{},
				Status:              TaskStatusPending,
				Attempts:            0,
			},
		}, ProjectBoardRoles{Planner: "claude-code", Executor: "claude-code"})
		out := FormatPlanOnly(board)
		for _, want := range []string{
			"[T1] schema migration (pending)",
			"add the exports table",
			"criteria:",
			"    migrations/001_exports.up.sql exists",
			"constraints:",
			"    do not touch auth schema",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q\noutput:\n%s", want, out)
			}
		}
	})

	t.Run("omits empty sections instead of printing headers with nothing under them", func(t *testing.T) {
		board := CreateBoard("g", []OrchestratorTask{
			stTask("T1", "t", "p", []string{}, TaskStatusPending, 0),
		}, ProjectBoardRoles{Planner: "claude-code", Executor: "claude-code"})
		out := FormatPlanOnly(board)
		if strings.Contains(out, "criteria:") || strings.Contains(out, "constraints:") {
			t.Errorf("empty sections must be omitted:\n%s", out)
		}
	})
}

func TestApplyHumanAnswer(t *testing.T) {
	askBoard := func() *ProjectBoard {
		return &ProjectBoard{
			Goal:      "g",
			CreatedAt: "2026-08-24T00:00:00Z",
			UpdatedAt: "2026-08-24T00:00:00Z",
			Roles:     ProjectBoardRoles{Planner: "claude-code", Executor: "claude-code"},
			Tasks: []OrchestratorTask{
				{
					ID:            "T1",
					Title:         "paused",
					Prompt:        "original contract",
					DependsOn:     []string{},
					Status:        TaskStatusAsk,
					Attempts:      1,
					FailureDetail: "needs human input: which API key env var?",
				},
				stTask("T2", "done", "p", []string{}, TaskStatusDone, 1),
			},
		}
	}

	t.Run("folds the answer into the contract and requeues the task", func(t *testing.T) {
		b := askBoard()
		r := ApplyHumanAnswer(b, "T1", "use ORG_API_KEY")
		if !r.OK {
			t.Fatalf("r.ok = false, want true (note %q)", r.Note)
		}
		if got := b.Tasks[0].Status; got != TaskStatusPending {
			t.Errorf("status = %q, want pending", got)
		}
		if !strings.Contains(b.Tasks[0].Prompt, "use ORG_API_KEY") {
			t.Errorf("prompt missing answer: %q", b.Tasks[0].Prompt)
		}
		if !strings.Contains(b.Tasks[0].Prompt, "which API key env var?") { // question kept as context
			t.Errorf("prompt missing question context: %q", b.Tasks[0].Prompt)
		}
		if b.Tasks[0].EvidenceGaps != nil {
			t.Errorf("evidenceGaps = %v, want cleared", b.Tasks[0].EvidenceGaps)
		}
	})

	t.Run("rejects unknown ids, non-ask tasks, and empty answers", func(t *testing.T) {
		if r := ApplyHumanAnswer(askBoard(), "TX", "yes"); r.OK {
			t.Error("unknown id must be rejected")
		}
		if r := ApplyHumanAnswer(askBoard(), "T2", "yes"); r.OK {
			t.Error("non-ask task must be rejected")
		}
		if r := ApplyHumanAnswer(askBoard(), "T1", "   "); r.OK {
			t.Error("empty answer must be rejected")
		}
	})

	// applyAnswerToRepo status-code contract (kept from the TS endpoint seam;
	// the vitest file covers it indirectly through applyHumanAnswer).
	t.Run("applyAnswerToRepo maps 400/404/409/200", func(t *testing.T) {
		repo := stTempRepo(t)
		if r := ApplyAnswerToRepo(repo, "T1", "  "); r.Status != 400 {
			t.Errorf("empty answer: status = %d, want 400", r.Status)
		}
		if r := ApplyAnswerToRepo(repo, "T1", "yes"); r.Status != 404 {
			t.Errorf("no board: status = %d, want 404", r.Status)
		}
		SaveBoard(repo, askBoard())
		if r := ApplyAnswerToRepo(repo, "T2", "yes"); r.Status != 409 {
			t.Errorf("non-ask task: status = %d, want 409", r.Status)
		}
		if r := ApplyAnswerToRepo(repo, "T1", "use ORG_API_KEY"); r.Status != 200 {
			t.Errorf("success: status = %d, want 200", r.Status)
		}
		b := LoadBoard(repo)
		if b == nil || b.Tasks[0].Status == TaskStatusAsk || !strings.Contains(b.Tasks[0].Prompt, "use ORG_API_KEY") {
			t.Errorf("board not requeued after endpoint answer: %+v", b)
		}
	})

	// CreateBoard timestamps (integration with internal/queue.NowIso).
	t.Run("createBoard stamps both timestamps from nowIso", func(t *testing.T) {
		b := CreateBoard("x", nil, ProjectBoardRoles{Planner: "omp", Executor: "omp"})
		if b.CreatedAt == "" || b.CreatedAt != b.UpdatedAt {
			t.Errorf("createdAt/updatedAt = %q/%q, want equal non-empty", b.CreatedAt, b.UpdatedAt)
		}
		if _, err := time.Parse("2006-01-02T15:04:05.000Z07:00", b.CreatedAt); err != nil {
			t.Errorf("createdAt not ISO millis: %v", err)
		}
	})
}
