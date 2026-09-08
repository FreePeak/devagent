package cli

// Go-only tests for the pipeline-family CLI wiring (actions_pipeline.go).
// The cross-runtime exit/stdout pins live in parity_wired_test.go; these
// cover behaviors that cannot run both sides hermetically (PRD mutation,
// worker-dispatch mapping, flag-default wiring, plan-only resume).

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/config"
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

// TestTaskPublishStageAfterAutoCleanup is the soak-169 regression (issue
// #238, BUG 1): with cleanup=auto a succeeded implement removes its
// worktree, so PublishStage used to see WorktreePath pointing at the
// removed dir and the old `WorktreePath == ""`-style guard silently
// skipped publishing — RunTask fell through to "no remote credentials;
// branch preserved locally" and the loop shipped no PR. The wiring must
// publish from the surviving run branch instead and surface the PR URL.
func TestTaskPublishStageAfterAutoCleanup(t *testing.T) {
	repo := t.TempDir()
	gitBin := "git"
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "init")

	wt := filepath.Join(repo, ".devagent-worktrees", "TASK-soakfix")
	run("worktree", "add", "-b", "devagent/TASK-soakfix", wt)
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("-C", wt, "add", "-A")
	run("-C", wt, "commit", "-m", "worker output")
	// cleanup=auto: the worktree is gone; only the run branch survives.
	run("worktree", "remove", "--force", wt)
	run("worktree", "prune")

	// The wiring under test: the real taskPublishStage with an EMPTY token
	// (the daemon env after the GITHUB_TOKEN cleanup) and stubbed remote
	// seams. The git seams stay real so the removed-worktree path runs.
	var pushed []string
	var prReqs []pipeline.TaskPublishPrRequest
	wiring := taskPublishWiring{
		Creds:      config.Credentials{},
		RepoPath:   repo,
		Prompt:     "Fix the soak publish path",
		BaseBranch: "main",
		PushBranch: func(_ string, branch string) error {
			pushed = append(pushed, branch)
			return nil
		},
		CreatePr: func(o pipeline.TaskPublishPrRequest) (string, error) {
			prReqs = append(prReqs, o)
			return "https://github.com/acme/repo/pull/77", nil
		},
	}

	var pubErr error
	url := taskPublishStage(wiring, pipeline.PublishImpl{OK: true, WorktreePath: wt}, &pubErr)
	if pubErr != nil {
		t.Fatalf("pubErr: %v", pubErr)
	}
	if url == "" {
		t.Fatal("publish after auto-cleanup returned no PR URL — the soak-169 silent skip is back (issue #238)")
	}
	if url != "https://github.com/acme/repo/pull/77" {
		t.Fatalf("url = %q", url)
	}
	if len(pushed) != 1 || pushed[0] != "devagent/TASK-soakfix" {
		t.Fatalf("pushed = %q, want [devagent/TASK-soakfix]", pushed)
	}
	if len(prReqs) != 1 || prReqs[0].Branch != "devagent/TASK-soakfix" {
		t.Fatalf("pr requests = %+v", prReqs)
	}

	// The RunTask level: with the wiring returning a URL, the result note is
	// "PR opened: ..." — never the misleading credentials note.
	res := pipeline.RunTask(pipeline.TaskOptions{Prompt: "Fix the soak publish path", RepoPath: repo, AutoPr: true}, pipeline.TaskDeps{
		ImplementStage: func(pipeline.TaskOptions, pipeline.TicketSpec, pipeline.RunLog) pipeline.TaskImplResult {
			return pipeline.TaskImplResult{OK: true, Worker: "omp", Attempts: 1, WorktreePath: wt}
		},
		PublishStage: func(_ pipeline.TaskOptions, _ pipeline.TicketSpec, impl pipeline.PublishImpl) string {
			u := taskPublishStage(wiring, impl, &pubErr)
			if u == "" {
				t.Fatal("publish stage returned empty URL for a removed-worktree impl")
			}
			return u
		},
	})
	if !res.OK || res.PRURL == "" || !strings.HasPrefix(res.Note, "PR opened: ") {
		t.Fatalf("RunTask result = %+v, want note 'PR opened: ...', not 'no remote credentials'", res)
	}
}
