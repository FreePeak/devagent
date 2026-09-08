package cli

// Go-only tests for the pipeline-family CLI wiring (actions_pipeline.go):
// behaviors that must run hermetically (PRD mutation, worker-dispatch
// mapping, flag-default wiring, plan-only resume).

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/orchestrator"
	"github.com/FreePeak/devagent/internal/pipeline"
)

// runCmdCapture runs the full CLI with the given argv and os.Stdout
// captured, returning the captured output.
func runCmdCapture(t *testing.T, args ...string) string {
	t.Helper()
	root := NewRoot()
	root.SetArgs(args)
	root.SilenceUsage = true

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)

	old := os.Stdout
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var out bytes.Buffer
		_, _ = io.Copy(&out, r)
		done <- out.String()
	}()
	execErr := root.Execute()
	_ = w.Close()
	os.Stdout = old
	captured := <-done
	if execErr != nil {
		t.Fatalf("execute %v: %v", args, execErr)
	}
	return captured
}

// TestBacklogCheckStrikeWritesPRD: --strike wraps confirmed-shipped backlog
// items in ~~ inside docs/PRD.md and prints the struck ids (the run's own
// PRD write; the parity harness cannot exercise it both sides because the
// mutation is order-dependent).
func TestBacklogCheckStrikeWritesPRD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVAGENT_HOME", home)
	repo := t.TempDir()
	writePRDFixture(t, repo,
		"- **Keep me open** — still current (Q40).\n"+
			"- **The finished widget** — done elsewhere (Q42).\n"+
			"> **Completed post-v0.3:** The finished widget shipped (abc123).\n")

	commandExitCode = nil
	out := runCmdCapture(t, "backlog-check", "Q40", "--strike", "--repo", repo)

	if commandExitCode == nil || *commandExitCode != 2 {
		t.Fatalf("exit code = %v, want 2 (offline: no merged-title evidence)", commandExitCode)
	}
	if !strings.Contains(out, "struck: Q42") {
		t.Fatalf("stdout missing struck line: %q", out)
	}
	prd, err := os.ReadFile(filepath.Join(repo, "docs", "PRD.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prd), "~~- **The finished widget**") {
		t.Fatalf("Q42 line must be struck in PRD:\n%s", prd)
	}
	if strings.Contains(string(prd), "~~- **Keep me open**") {
		t.Fatalf("Q40 must stay open:\n%s", prd)
	}
}

// TestMapDispatchRequest pins the orchestrator.WorkerDispatchRequest ->
// workers.WorkerSpawnOptions mapping (watchdog ledger identity, resilience
// knobs, herdr routing) without spawning a worker.
func TestMapDispatchRequest(t *testing.T) {
	req := orchestrator.WorkerDispatchRequest{
		Prompt: "do the thing", Cwd: "/tmp/repo", TimeoutMs: 1500, Attempt: 2,
		WatchdogRepoPath: "/tmp/repo", WatchdogTaskID: "TASK-1", WatchdogWorker: "omp",
		Model: "prov/model", Variant: "think",
		APIMaxAttempts: 4, NoProgressTimeoutMs: 90_000, ColdStartTimeoutMs: 45_000, Herdr: true,
	}
	w, opts, err := mapDispatchRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if w == nil || w.Name() != "omp" {
		t.Fatalf("worker = %v, want omp adapter", w)
	}
	if opts.Prompt != req.Prompt || opts.Cwd != req.Cwd || opts.TimeoutMs != 1500 {
		t.Fatalf("core mapping = %+v", opts)
	}
	if opts.Model != "prov/model" || opts.Variant != "think" || opts.ColdStartTimeoutMs != 45_000 {
		t.Fatalf("knob mapping = %+v", opts)
	}
	if opts.APIMaxAttempts == nil || *opts.APIMaxAttempts != 4 {
		t.Fatalf("apiMaxAttempts = %v, want 4", opts.APIMaxAttempts)
	}
	if opts.NoProgressTimeoutMs == nil || *opts.NoProgressTimeoutMs != 90_000 {
		t.Fatalf("noProgressTimeoutMs = %v, want 90000", opts.NoProgressTimeoutMs)
	}
	if opts.Herdr == nil || !*opts.Herdr {
		t.Fatalf("herdr = %v, want true", opts.Herdr)
	}
	if opts.WatchdogLedger == nil ||
		opts.WatchdogLedger.RepoPath != "/tmp/repo" || opts.WatchdogLedger.TaskId != "TASK-1" ||
		opts.WatchdogLedger.Attempt != 2 || opts.WatchdogLedger.Worker != "omp" {
		t.Fatalf("watchdog ledger = %+v", opts.WatchdogLedger)
	}

	// Zero-valued knobs stay unset (TS undefined), herdr unset stays nil so
	// the adapter resolves via env.
	w2, opts2, err := mapDispatchRequest(orchestrator.WorkerDispatchRequest{Prompt: "x", Cwd: "/tmp", WatchdogWorker: "omp"})
	if err != nil {
		t.Fatal(err)
	}
	if opts2.APIMaxAttempts != nil || opts2.NoProgressTimeoutMs != nil || opts2.Herdr != nil {
		t.Fatalf("unset knobs must stay nil: %+v", opts2)
	}
	if opts2.WatchdogLedger == nil || opts2.WatchdogLedger.Attempt != 0 {
		t.Fatalf("watchdog ledger identity = %+v", opts2.WatchdogLedger)
	}
	_ = w2

	if _, _, err := mapDispatchRequest(orchestrator.WorkerDispatchRequest{WatchdogWorker: "bogus-worker"}); err == nil {
		t.Fatal("unknown worker must error (TS getWorker throws)")
	}
}

// TestPipelineFlagDefaults pins the TS option defaults the wiring applies to
// the newly wired commands.
func TestPipelineFlagDefaults(t *testing.T) {
	root := NewRoot()
	cases := map[string]map[string]string{
		"reap-stale":  {"older-than": "600000"},
		"consume":     {"once": "true"},
		"fleet":       {"concurrency": "2"},
		"orchestrate": {"concurrency": "2", "max-task-retries": "1", "max-recoveries": "1", "max-total-attempts": "0"},
	}
	for name, want := range cases {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatalf("find %s: %v", name, err)
		}
		for flag, def := range want {
			f := cmd.Flags().Lookup(flag)
			if f == nil {
				t.Fatalf("%s: flag --%s missing", name, flag)
			}
			if f.DefValue != def {
				t.Errorf("%s --%s default = %q, want %q", name, flag, f.DefValue, def)
			}
		}
	}
	// The variadic flags must be string arrays (repeatable), not strings.
	fleet, _, _ := root.Find([]string{"fleet"})
	for _, flag := range []string{"ticket", "repo"} {
		f := fleet.Flags().Lookup(flag)
		if f == nil || f.Value.Type() != "stringArray" {
			t.Errorf("fleet --%s type = %v, want stringArray", flag, f)
		}
	}
	orch, _, _ := root.Find([]string{"orchestrate"})
	if f := orch.Flags().Lookup("answer"); f == nil || f.Value.Type() != "stringArray" {
		t.Errorf("orchestrate --answer must be repeatable stringArray")
	}
}

// TestOrchestratePlanOnlyResumesBoard: --resume --plan-only with an existing
// board prints the contract preview and re-persists the board without
// dispatching any worker (validate-before-spend, hermetic).
func TestOrchestratePlanOnlyResumesBoard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVAGENT_HOME", home)
	repo := t.TempDir()
	board := orchestrator.CreateBoard("ship the thing", []orchestrator.OrchestratorTask{{
		ID: "T1", Title: "First", Prompt: "do it", Status: orchestrator.TaskStatusPending,
	}}, orchestrator.ProjectBoardRoles{Planner: "omp", Executor: "omp"})
	orchestrator.SaveBoard(repo, board)

	commandExitCode = nil
	out := runCmdCapture(t, "orchestrate", "--goal", "ship the thing", "--repo", repo, "--resume", "--plan-only")

	if !strings.Contains(out, "Plan-only: board saved at "+filepath.Join(repo, ".devagent-project.json")) {
		t.Fatalf("plan-only banner missing: %q", out)
	}
	if !strings.Contains(out, "First") {
		t.Fatalf("plan preview must include the task title: %q", out)
	}
	if commandExitCode != nil {
		t.Fatalf("plan-only must not set an exit code, got %d", *commandExitCode)
	}
	if _, err := os.Stat(filepath.Join(repo, ".devagent-project.json")); err != nil {
		t.Fatalf("board must persist: %v", err)
	}
}

// writePRDFixture writes a minimal docs/PRD.md whose Phase 4 current-backlog
// section holds the given bullet lines (the heading must case-insensitively
// contain "current backlog" + "phase 4" for the parser to enter the section).
// Relocated from the deleted parity_wired_test.go (Node retirement, #205).
func writePRDFixture(t *testing.T, dir, items string) {
	t.Helper()
	prd := "## Phase 4 — current backlog\n\n" + items
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "PRD.md"), []byte(prd), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTaskPublishStagePublishesFromRunBranch pins the LIVE publish wiring
// of `devagent task` (issue #238, soak-169 evidence): after auto-cleanup
// removed the worktree, publish must consult the surviving run branch and
// return a PR URL — with NO GITHUB_TOKEN (the soak environment's state),
// the previous wiring short-circuited to "" before publish ever ran.
func TestTaskPublishStagePublishesFromRunBranch(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "init")
	bare := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", bare).CombinedOutput(); err != nil {
		t.Fatalf("bare origin init: %v\n%s", err, out)
	}
	run("remote", "add", "origin", bare)

	wt := filepath.Join(repo, ".devagent-worktrees", "TASK-1")
	run("worktree", "add", "-b", "devagent/TASK-1", wt)
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("-C", wt, "add", "-A")
	run("-C", wt, "commit", "-m", "worker changes")
	// cleanup=auto: remove the worktree, the run branch survives in the
	// main repo (already pushed by FinalizeRunWorktree in the live path).
	run("worktree", "remove", "--force", wt)
	run("worktree", "prune")

	// Fake gh on PATH: `gh pr create` echoes a PR URL (lastNonEmptyLine).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(`#!/bin/sh
echo "gh $*" >> "${GH_LOG:?}"
case "$1 $2" in
  "pr create") echo "https://example.fake/pull/1"; exit 0 ;;
esac
exit 0
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("GH_LOG", filepath.Join(repo, "gh-calls.log"))
	// The soak condition: no GitHub token in the environment.
	t.Setenv("GITHUB_TOKEN", "")

	var pubErr error
	publish := taskPublishStage(repo, "Fix the thing", "main", false, nil, &pubErr)
	url := publish(pipeline.TaskOptions{RepoPath: repo, AutoPr: true}, pipeline.TicketSpec{}, pipeline.PublishImpl{OK: true, WorktreePath: wt})
	if pubErr != nil {
		t.Fatalf("publish error: %v", pubErr)
	}
	if url != "https://example.fake/pull/1" {
		t.Fatalf("url = %q, want the PR opened from the run branch", url)
	}
	calls, err := os.ReadFile(filepath.Join(repo, "gh-calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "-H devagent/TASK-1") {
		t.Fatalf("gh calls must create the PR for the run branch:\n%s", calls)
	}
}
