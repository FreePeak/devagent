// Package file mirrors test/orchestrator-merge.test.ts (FR-GO-07, issue #194).

package orchestrator

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/FreePeak/devagent/internal/git"
	"github.com/FreePeak/devagent/internal/ledger"
)

func mergeTestBoard(tasks []OrchestratorTask) ProjectBoard {
	// TS: roles { planner: 'claude-code', executor: 'opencode' } — the Go
	// ProjectBoardRoles has no executor field; tests never assert roles.
	return ProjectBoard{Goal: "g", Roles: ProjectBoardRoles{Planner: "claude-code"}, Tasks: tasks}
}

func topoTask(id string, dependsOn []string, status TaskStatus, attempts int) OrchestratorTask {
	return OrchestratorTask{ID: id, Title: "", Prompt: "", DependsOn: dependsOn, Status: status, Attempts: attempts}
}

func TestTopoOrder(t *testing.T) {
	t.Run("emits dependencies before dependents", func(t *testing.T) {
		b := mergeTestBoard([]OrchestratorTask{
			topoTask("T3", []string{"T1", "T2"}, TaskStatusDone, 1),
			topoTask("T1", []string{}, TaskStatusDone, 1),
			topoTask("T2", []string{"T1"}, TaskStatusDone, 2),
		})
		order := TopoOrder(b)
		idx := map[string]int{}
		for i, id := range order {
			idx[id] = i
		}
		if idx["T1"] >= idx["T2"] {
			t.Fatalf("T1 must precede T2, order=%v", order)
		}
		if idx["T2"] >= idx["T3"] {
			t.Fatalf("T2 must precede T3, order=%v", order)
		}
	})

	t.Run("includes only done tasks in merge candidates when filtered by caller", func(t *testing.T) {
		// mergeProjectBranches filters to done; topoOrder covers all — contract check
		b := mergeTestBoard([]OrchestratorTask{
			topoTask("A", []string{}, TaskStatusDone, 1),
			topoTask("B", []string{"A"}, TaskStatusFailed, 1),
		})
		done := map[string]bool{}
		for _, task := range b.Tasks {
			if task.Status == TaskStatusDone {
				done[task.ID] = true
			}
		}
		got := []string{}
		for _, id := range TopoOrder(b) {
			if done[id] {
				got = append(got, id)
			}
		}
		if len(got) != 1 || got[0] != "A" {
			t.Fatalf("expected [A], got %v", got)
		}
	})
}

func TestPerTaskPrPublished(t *testing.T) {
	t.Run("is true when a done task published its per-task PR", func(t *testing.T) {
		b := mergeTestBoard([]OrchestratorTask{
			topoTask("T1", nil, TaskStatusDone, 1),
		})
		b.Tasks[0].PrURL = "https://github.com/o/r/pull/1"
		if !PerTaskPrPublished(b) {
			t.Fatal("expected true")
		}
	})

	t.Run("is false when no done task has a PR URL (legacy merge-back still owns integration)", func(t *testing.T) {
		b := mergeTestBoard([]OrchestratorTask{
			topoTask("T1", nil, TaskStatusDone, 1),
			topoTask("T2", nil, TaskStatusDone, 1),
		})
		if PerTaskPrPublished(b) {
			t.Fatal("expected false")
		}
	})

	t.Run("ignores empty PR URLs", func(t *testing.T) {
		b := mergeTestBoard([]OrchestratorTask{
			topoTask("T1", nil, TaskStatusDone, 1),
		})
		b.Tasks[0].PrURL = ""
		if PerTaskPrPublished(b) {
			t.Fatal("expected false")
		}
	})

	t.Run("ignores PR URLs on tasks that are not done", func(t *testing.T) {
		b := mergeTestBoard([]OrchestratorTask{
			topoTask("T1", nil, TaskStatusFailed, 1),
		})
		b.Tasks[0].PrURL = "https://github.com/o/r/pull/1"
		if PerTaskPrPublished(b) {
			t.Fatal("expected false")
		}
	})

	t.Run("is false for an empty board", func(t *testing.T) {
		if PerTaskPrPublished(mergeTestBoard(nil)) {
			t.Fatal("expected false")
		}
	})
}

// initRepo creates a temp git repo with one committed file (TS initRepo).
func initRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "init")
	return repo
}

func stashRows(t *testing.T, repo string) []map[string]any {
	t.Helper()
	file := filepath.Join(repo, ledger.LedgerDir, "events.jsonl")
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	rows := []map[string]any{}
	for _, line := range splitLines(string(data)) {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad ledger row %q: %v", line, err)
		}
		if r["event"] == "merge-back-stash" {
			rows = append(rows, r)
		}
	}
	return rows
}

// splitLines splits and drops empty trailing lines.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func TestRestoreAutoStash(t *testing.T) {
	t.Run("emits a restored row when the stash pops back cleanly", func(t *testing.T) {
		repo := initRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("local work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sha, err := git.StashMainWorktree(repo, "devagent auto-stash before merge")
		if err != nil || sha == "" {
			t.Fatalf("stash: sha=%q err=%v", sha, err)
		}

		if !RestoreAutoStash(repo, sha) {
			t.Fatal("expected restore to succeed")
		}
		got, err := os.ReadFile(filepath.Join(repo, "f.txt"))
		if err != nil || string(got) != "local work\n" {
			t.Fatalf("f.txt = %q, err=%v", got, err)
		}

		rows := stashRows(t, repo)
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		r := rows[0]
		if r["kind"] != "event" || r["event"] != "merge-back-stash" ||
			r["taskId"] != "merge-back" || r["attempt"] != float64(1) ||
			r["stashSha"] != sha || r["outcome"] != "restored" {
			t.Fatalf("unexpected row: %v", r)
		}
		if _, ok := r["ts"].(string); !ok {
			t.Fatalf("ts must be a string: %v", r["ts"])
		}
		if _, ok := r["detail"].(string); !ok {
			t.Fatalf("detail must be a string: %v", r["detail"])
		}
	})

	t.Run("emits a retained row and keeps the stash when the pop conflicts", func(t *testing.T) {
		repo := initRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("local work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sha, err := git.StashMainWorktree(repo, "devagent auto-stash before merge")
		if err != nil || sha == "" {
			t.Fatalf("stash: sha=%q err=%v", sha, err)
		}
		// Merge-back lands a conflicting change to the same file: apply fails.
		if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("merged work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = repo
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		run("add", ".")
		run("commit", "-m", "merge-back")

		if RestoreAutoStash(repo, sha) {
			t.Fatal("expected restore to fail on conflict")
		}
		// User work is never dropped: the stash entry survives for manual recovery
		cmd := exec.Command("git", "stash", "list", "--format=%H")
		cmd.Dir = repo
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if trimSpace(string(out)) != sha {
			t.Fatalf("stash list = %q, want %q", out, sha)
		}

		rows := stashRows(t, repo)
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		r := rows[0]
		if r["kind"] != "event" || r["event"] != "merge-back-stash" ||
			r["taskId"] != "merge-back" || r["attempt"] != float64(1) ||
			r["stashSha"] != sha || r["outcome"] != "retained" {
			t.Fatalf("unexpected row: %v", r)
		}
	})
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\n' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
