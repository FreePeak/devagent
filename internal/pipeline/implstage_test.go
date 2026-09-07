// implstage_test.go — hermetic tests for the implement-stage port: the
// preflight shape, the FR-IMPL retry loop (spawner/gate/backoff seams), the
// backoff and proxy-state helpers, finalize/e2e behavior over real local
// git, the publish stage over real git with a fake `gh` on PATH, and the
// G4 async-review wiring. No network, no real worker CLIs.

package pipeline

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/gates"
	"github.com/FreePeak/devagent/internal/git"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/scout"
	"github.com/FreePeak/devagent/internal/workers"
)

// --- helpers (imp-prefixed: same-package collision rule) ---

type impLogEntry struct {
	Level   string
	Stage   ledger.RunStage
	Message string
	Data    []ledger.KV
}

// impCaptureLog is a RunLog capturing entries for assertions.
type impCaptureLog struct {
	Entries []impLogEntry
}

func (l *impCaptureLog) add(level string, stage ledger.RunStage, message string, data []ledger.KV) {
	l.Entries = append(l.Entries, impLogEntry{Level: level, Stage: stage, Message: message, Data: data})
}

func (l *impCaptureLog) Info(stage ledger.RunStage, message string, data []ledger.KV) {
	l.add("info", stage, message, data)
}

func (l *impCaptureLog) Warn(stage ledger.RunStage, message string, data []ledger.KV) {
	l.add("warn", stage, message, data)
}

func (l *impCaptureLog) Error(stage ledger.RunStage, message string, data []ledger.KV) {
	l.add("error", stage, message, data)
}

// has reports whether any entry of the given level contains substr.
func (l *impCaptureLog) has(level, substr string) bool {
	for _, e := range l.Entries {
		if e.Level == level && strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}

// kvOf returns the data value recorded for the first matching entry.
func (l *impCaptureLog) kvOf(level, messageSubstr, key string) (any, bool) {
	for _, e := range l.Entries {
		if e.Level != level || !strings.Contains(e.Message, messageSubstr) {
			continue
		}
		for _, kv := range e.Data {
			if kv.Key == key {
				return kv.Value, true
			}
		}
	}
	return nil, false
}

// impTestPlan is the shared plan fixture.
func impTestPlan() ImplementationPlan {
	return ImplementationPlan{
		Ticket: scout.TicketSpec{
			ID:                 "T-IMP",
			Title:              "Impl stage test",
			Description:        "desc",
			Labels:             []string{},
			AcceptanceCriteria: []string{},
		},
		Classification: "endpoint-only",
		Tasks:          []string{"do the thing"},
	}
}

// impSwapSpawner overrides impResolveSpawner for the duration of a test.
func impSwapSpawner(f func(WorkerName) (func(workers.WorkerSpawnOptions) workers.WorkerResult, error)) (restore func()) {
	old := impResolveSpawner
	impResolveSpawner = f
	return func() { impResolveSpawner = old }
}

// impGit runs a git command in dir, failing the test on error.
func impGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v: %s", args, err, out)
	}
}

// impInitGitRepo creates a git repo with a bare origin and one seed commit
// on main. No devagent.json/package.json/go.mod, so RunTestGate skips.
func impInitGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	impGit(t, repo, "init", "-b", "main")
	impGit(t, repo, "config", "user.email", "test@devagent.local")
	impGit(t, repo, "config", "user.name", "DevAgent Test")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	impGit(t, repo, "add", "-A")
	impGit(t, repo, "commit", "-m", "seed")
	origin := t.TempDir()
	impGit(t, origin, "init", "--bare", "-b", "main")
	impGit(t, repo, "remote", "add", "origin", origin)
	impGit(t, repo, "push", "-u", "origin", "main")
	return repo
}

func impGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestImpImplementStagePreflightConfigFailure(t *testing.T) {
	log := &impCaptureLog{}
	repo := t.TempDir()
	// omp rejects non-provider-qualified ids ("coding"), so the preflight
	// must fail at the gate before any worktree/worker spend.
	cfg := StageConfig{RepoPath: repo, MaxLoops: 3, TimeoutMs: 1000, Worker: "omp", Model: "coding"}
	res, err := ImplementStage(cfg, impTestPlan(), log)
	if err != nil {
		t.Fatalf("preflight failure must not be an error: %v", err)
	}
	if res.OK || res.Worker != "omp" || res.Attempts != 0 || res.FailureClass != "config" {
		t.Fatalf("bad preflight result: %+v", res)
	}
	if !log.has("error", "Dispatch preflight failed: ") {
		t.Fatalf("missing preflight error log: %+v", log.Entries)
	}

	// Note: the `worker === 'both'` canonicalization branch in the preflight
	// result is unreachable through model validation — claude-code and
	// opencode are passthrough validators, so 'both' never fails the gate.
}

// --- retry loop ---

func impBaseRetryEnv(repo string) impRetryEnv {
	return impRetryEnv{
		workerName:   "claude-code",
		repoPath:     repo,
		cwd:          repo,
		worktreePath: "/wt",
		prompt:       "PROMPT",
		maxAttempts:  3,
		noProgressMs: 600000,
		coldStartMs:  90000,
	}
}

func TestImpRetryLoopSuccessFirstAttempt(t *testing.T) {
	repo := t.TempDir()
	log := &impCaptureLog{}
	spawns := 0
	env := impBaseRetryEnv(repo)
	env.spawn = func(opts workers.WorkerSpawnOptions) workers.WorkerResult {
		spawns++
		if opts.Prompt != "PROMPT" {
			t.Errorf("attempt 1 must use the base prompt, got %q", opts.Prompt)
		}
		if opts.WatchdogLedger == nil || opts.WatchdogLedger.RepoPath != repo || opts.WatchdogLedger.TaskId != "T-IMP" ||
			opts.WatchdogLedger.Attempt != 1 || opts.WatchdogLedger.Worker != "claude-code" {
			t.Errorf("bad watchdog ledger: %+v", opts.WatchdogLedger)
		}
		if opts.NoProgressTimeoutMs == nil || *opts.NoProgressTimeoutMs != 600000 {
			t.Errorf("no-progress timeout must default through: %+v", opts.NoProgressTimeoutMs)
		}
		return workers.WorkerResult{ExitCode: 0, ResultText: "done", DurationMs: 42}
	}
	env.testGate = func(string, int) (gates.GateResult, error) {
		return gates.GateResult{Passed: true, Detail: "all good\nflaky tail"}, nil
	}
	env.backoff = func(int) int { return 0 }
	apiMax := 3.0
	env.apiMaxAttempts = &apiMax
	res, succeeded, err := impRunRetryLoop(env, impTestPlan(), StageConfig{TimeoutMs: 1000}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !succeeded || !res.OK || res.Attempts != 1 || res.WorktreePath != "/wt" || res.Worker != "claude-code" {
		t.Fatalf("bad success result: %+v succeeded=%v", res, succeeded)
	}
	if spawns != 1 {
		t.Fatalf("expected 1 spawn, got %d", spawns)
	}
	if !log.has("info", "Worker claude-code attempt 1/3") {
		t.Fatalf("missing attempt log: %+v", log.Entries)
	}
	if v, ok := log.kvOf("info", "Attempt 1 finished", "exitCode"); !ok || v != 0 {
		t.Fatalf("missing exitCode KV: %v %v", v, ok)
	}
	if v, ok := log.kvOf("info", "Attempt 1 test gate", "detail"); !ok || v != "all good" {
		t.Fatalf("test-gate detail KV must be first line: %v %v", v, ok)
	}
	if !log.has("info", "Attempt 1 test gate: passed") {
		t.Fatalf("missing gate log: %+v", log.Entries)
	}
}

func TestImpRetryLoopTransientThenSuccess(t *testing.T) {
	repo := t.TempDir()
	log := &impCaptureLog{}
	spawns := 0
	env := impBaseRetryEnv(repo)
	env.spawn = func(workers.WorkerSpawnOptions) workers.WorkerResult {
		spawns++
		if spawns == 1 {
			return workers.WorkerResult{ExitCode: 124, TimedOut: true, DurationMs: 1000}
		}
		return workers.WorkerResult{ExitCode: 0, ResultText: "recovered"}
	}
	env.testGate = func(string, int) (gates.GateResult, error) {
		return gates.GateResult{Passed: true}, nil
	}
	env.backoff = func(int) int { return 0 }
	res, succeeded, err := impRunRetryLoop(env, impTestPlan(), StageConfig{TimeoutMs: 1000}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !succeeded || !res.OK || res.Attempts != 1 {
		t.Fatalf("transient retries must not consume the logic budget: %+v", res)
	}
	if spawns != 2 {
		t.Fatalf("expected 2 spawns, got %d", spawns)
	}
	if !log.has("warn", "Transient infra failure, retrying (infra retry 1)") {
		t.Fatalf("missing infra warn: %+v", log.Entries)
	}
	if _, err := os.Stat(impProxyStatePath(repo)); err != nil {
		t.Fatalf("transient class must land in proxy-state.json: %v", err)
	}
}

func TestImpRetryLoopNonRetryableUsesRepairPrompt(t *testing.T) {
	repo := t.TempDir()
	log := &impCaptureLog{}
	var prompts []string
	spawns := 0
	env := impBaseRetryEnv(repo)
	env.spawn = func(opts workers.WorkerSpawnOptions) workers.WorkerResult {
		spawns++
		prompts = append(prompts, opts.Prompt)
		if spawns == 1 {
			return workers.WorkerResult{ExitCode: 1, ResultText: "credit balance is too low"}
		}
		return workers.WorkerResult{ExitCode: 0, ResultText: "ok"}
	}
	env.testGate = func(string, int) (gates.GateResult, error) {
		return gates.GateResult{Passed: true}, nil
	}
	env.backoff = func(int) int { return 0 }
	res, succeeded, err := impRunRetryLoop(env, impTestPlan(), StageConfig{TimeoutMs: 1000}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !succeeded || !res.OK || spawns != 2 {
		t.Fatalf("bad result: %+v spawns=%d", res, spawns)
	}
	if prompts[1] == "PROMPT" {
		t.Fatalf("attempt 2 must use the repair prompt")
	}
}

func TestImpRetryLoopWorkerErrorExhaustsBudget(t *testing.T) {
	repo := t.TempDir()
	log := &impCaptureLog{}
	spawns := 0
	env := impBaseRetryEnv(repo)
	env.maxAttempts = 2
	env.spawn = func(workers.WorkerSpawnOptions) workers.WorkerResult {
		spawns++
		return workers.WorkerResult{ExitCode: 1, ResultText: "segfault in logic"}
	}
	env.testGate = func(string, int) (gates.GateResult, error) {
		t.Fatal("gate must not run when the worker never succeeds")
		return gates.GateResult{}, nil
	}
	env.backoff = func(int) int { return 0 }
	res, succeeded, err := impRunRetryLoop(env, impTestPlan(), StageConfig{TimeoutMs: 1000}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if succeeded || res.OK || res.Attempts != 2 || res.FailureClass != "worker-error" || res.WorktreePath != "/wt" {
		t.Fatalf("bad exhaustion result: %+v succeeded=%v", res, succeeded)
	}
	if spawns != 2 {
		t.Fatalf("expected 2 spawns, got %d", spawns)
	}
}

func TestImpRetryLoopInfiniteModeBreaksAtMaxLoops(t *testing.T) {
	for name, apiMax := range map[string]*float64{
		"nil":      nil,
		"infinity": func() *float64 { v := math.Inf(1); return &v }(),
	} {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			log := &impCaptureLog{}
			spawns := 0
			env := impBaseRetryEnv(repo)
			env.maxAttempts = 1
			env.apiMaxAttempts = apiMax
			env.spawn = func(workers.WorkerSpawnOptions) workers.WorkerResult {
				spawns++
				return workers.WorkerResult{ExitCode: 1, ResultText: "plain failure"}
			}
			env.testGate = func(string, int) (gates.GateResult, error) {
				return gates.GateResult{Passed: true}, nil
			}
			env.backoff = func(int) int { return 0 }
			res, _, err := impRunRetryLoop(env, impTestPlan(), StageConfig{TimeoutMs: 1000}, log)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.OK || res.Attempts != 1 || res.FailureClass != "worker-error" {
				t.Fatalf("infinite mode must break at maxLoops when no infra retries: %+v", res)
			}
			if spawns != 1 {
				t.Fatalf("expected 1 spawn, got %d", spawns)
			}
			if !log.has("info", "Worker claude-code attempt 1/∞") {
				t.Fatalf("infinite mode must render ∞: %+v", log.Entries)
			}
		})
	}
}

func TestImpRetryLoopGateFailure(t *testing.T) {
	repo := t.TempDir()
	log := &impCaptureLog{}
	env := impBaseRetryEnv(repo)
	env.maxAttempts = 1
	env.spawn = func(workers.WorkerSpawnOptions) workers.WorkerResult {
		return workers.WorkerResult{ExitCode: 0, ResultText: "worker happy"}
	}
	env.testGate = func(string, int) (gates.GateResult, error) {
		return gates.GateResult{Passed: false, Detail: "1 test failed"}, nil
	}
	env.backoff = func(int) int { return 0 }
	res, succeeded, err := impRunRetryLoop(env, impTestPlan(), StageConfig{TimeoutMs: 1000}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if succeeded || res.OK || res.Attempts != 1 || res.FailureClass != "test-gate" {
		t.Fatalf("bad gate-failure result: %+v succeeded=%v", res, succeeded)
	}
	if !log.has("info", "Attempt 1 test gate: failed") {
		t.Fatalf("missing gate-failed log: %+v", log.Entries)
	}
}

// --- backoff ---

func TestImpBackoffDelayBounds(t *testing.T) {
	for i := 0; i < 50; i++ {
		v := impBackoffDelay(1)
		if v < 1500 || v > 2500 {
			t.Fatalf("attempt 1 out of base jitter window: %d", v)
		}
		v0 := impBackoffDelay(0)
		if v0 < 1500 || v0 > 2500 {
			t.Fatalf("attempt 0 must be treated as 1: %d", v0)
		}
		// TS caps raw at 60000 then jitters ±25% — the cap applies to the
		// pre-jitter value, so the post-jitter ceiling is 75000.
		if v := impBackoffDelay(50); v < 45000 || v > 75000 {
			t.Fatalf("cap+jitter window violated: %d", v)
		}
	}
}

// --- proxy state ---

func TestImpRecordTransientClass(t *testing.T) {
	repo := t.TempDir()
	if got := impRecordTransientClass(repo, "hello world"); got != nil {
		t.Fatalf("non-transient text must not record: %+v", got)
	}
	if _, err := os.Stat(impProxyStatePath(repo)); !os.IsNotExist(err) {
		t.Fatalf("no state file must be written for non-transient text")
	}

	rec := impRecordTransientClass(repo, "rate limit exceeded")
	if rec == nil || rec.Class != "rate-limit" {
		t.Fatalf("bad transient record: %+v", rec)
	}
	raw, err := os.ReadFile(impProxyStatePath(repo))
	if err != nil {
		t.Fatal(err)
	}
	var state impProxyState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Circuit != "closed" || state.LastTransient == nil || state.LastTransient.Excerpt != "rate limit exceeded" {
		t.Fatalf("bad state: %+v", state)
	}
	if len(state.UpdatedAt) != 24 || !strings.HasSuffix(state.UpdatedAt, "Z") {
		t.Fatalf("updatedAt must be ISO with millis: %q", state.UpdatedAt)
	}

	// Whitespace collapses in the excerpt (classification still sees the
	// raw text, so keep the pattern matchable).
	rec = impRecordTransientClass(repo, "rate limit\n\texceeded again")
	if rec == nil || rec.Excerpt != "rate limit exceeded again" {
		t.Fatalf("excerpt must collapse whitespace: %+v", rec)
	}

	// Pre-existing circuit/probe state is preserved.
	pre := `{"circuit":"open","circuitChangedAt":"2026-01-01T00:00:00.000Z","lastProbe":{"ok":false,"at":"2026-01-01T00:00:00.000Z","detail":"x"},"updatedAt":"2026-01-01T00:00:00.000Z"}`
	if err := os.WriteFile(impProxyStatePath(repo), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := impRecordTransientClass(repo, "429 too many requests"); rec == nil {
		t.Fatalf("transient text must record")
	}
	raw, err = os.ReadFile(impProxyStatePath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Circuit != "open" || state.LastProbe == nil || state.LastProbe.Ok {
		t.Fatalf("pre-existing circuit/probe must be preserved: %+v", state)
	}
	if state.LastTransient == nil || state.LastTransient.Class != "rate-limit" {
		t.Fatalf("new transient must overwrite lastTransient: %+v", state)
	}
}

// --- finalize (real local git) ---

func TestImpFinalizeWorktree(t *testing.T) {
	repo := impInitGitRepo(t)
	plan := ImplementationPlan{Ticket: scout.TicketSpec{ID: "T-FIN", Title: "Fin"}}
	wt, err := git.CreateWorktree(repo, "T-FIN")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.WorktreePath, "w.txt"), []byte("w"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := &impCaptureLog{}
	impFinalizeWorktree(StageConfig{RepoPath: repo, Cleanup: "always"}, plan, wt.WorktreePath, true, log)
	if _, err := os.Stat(wt.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("always-cleanup must remove the worktree: %v", err)
	}
	if !log.has("info", "Auto-cleanup: worktree removed (changes snapshotted to run branch, run branch pushed to origin): ") {
		t.Fatalf("missing removed log: %+v", log.Entries)
	}

	// Keep preserves the tree.
	repo2 := impInitGitRepo(t)
	plan2 := ImplementationPlan{Ticket: scout.TicketSpec{ID: "T-KEEP", Title: "Keep"}}
	wt2, err := git.CreateWorktree(repo2, "T-KEEP")
	if err != nil {
		t.Fatal(err)
	}
	log2 := &impCaptureLog{}
	impFinalizeWorktree(StageConfig{RepoPath: repo2, Cleanup: CleanupKeep}, plan2, wt2.WorktreePath, true, log2)
	if _, err := os.Stat(wt2.WorktreePath); err != nil {
		t.Fatalf("keep must preserve the worktree: %v", err)
	}
	if !log2.has("info", "Worktree preserved for inspection: ") {
		t.Fatalf("missing preserved log: %+v", log2.Entries)
	}
}

// --- end-to-end ImplementStage over real git with a fake worker ---

func TestImpImplementStageEndToEnd(t *testing.T) {
	repo := impInitGitRepo(t)
	restore := impSwapSpawner(func(WorkerName) (func(workers.WorkerSpawnOptions) workers.WorkerResult, error) {
		return func(workers.WorkerSpawnOptions) workers.WorkerResult {
			return workers.WorkerResult{ExitCode: 0, ResultText: "ok", DurationMs: 5}
		}, nil
	})
	defer restore()
	log := &impCaptureLog{}
	cfg := StageConfig{RepoPath: repo, MaxLoops: 1, TimeoutMs: 5000, Worker: "claude-code", Cleanup: "always"}
	res, err := ImplementStage(cfg, impTestPlan(), log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.OK || res.Attempts != 1 || res.Worker != "claude-code" || res.WorktreePath == "" {
		t.Fatalf("bad e2e result: %+v", res)
	}
	if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("cleanup=always must remove the worktree: %v", err)
	}
	if !log.has("info", "Worktree ready: ") {
		t.Fatalf("missing worktree log: %+v", log.Entries)
	}
	if !log.has("info", "Auto-cleanup: worktree removed") {
		t.Fatalf("missing cleanup log: %+v", log.Entries)
	}

	// Failed run with default (auto) cleanup preserves the tree.
	repo2 := impInitGitRepo(t)
	restore2 := impSwapSpawner(func(WorkerName) (func(workers.WorkerSpawnOptions) workers.WorkerResult, error) {
		return func(workers.WorkerSpawnOptions) workers.WorkerResult {
			return workers.WorkerResult{ExitCode: 1, ResultText: "kaput"}
		}, nil
	})
	defer restore2()
	log2 := &impCaptureLog{}
	cfg2 := StageConfig{RepoPath: repo2, MaxLoops: 1, TimeoutMs: 5000, Worker: "claude-code"}
	res2, err := ImplementStage(cfg2, impTestPlan(), log2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res2.OK || res2.FailureClass != "worker-error" || res2.Attempts != 1 {
		t.Fatalf("bad failure result: %+v", res2)
	}
	if _, err := os.Stat(res2.WorktreePath); err != nil {
		t.Fatalf("failed auto run must preserve the worktree: %v", err)
	}
	if !log2.has("info", "Worktree preserved for inspection: ") {
		t.Fatalf("missing preserved log: %+v", log2.Entries)
	}
}

// --- G4 wiring (real local git) ---

func TestImpRunGateG4(t *testing.T) {
	repo := impInitGitRepo(t)
	impGit(t, repo, "checkout", "-b", "devagent/G4-1")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "a.ts"), []byte("fetch(url).then((r) => r.json());\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	impGit(t, repo, "add", "-A")
	impGit(t, repo, "commit", "-m", "hazard")
	res, err := impRunGateG4(repo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || res.Skipped {
		t.Fatalf("hazard must block: %+v", res)
	}
	if res.Detail != "1 changed file(s), 1 finding(s) (blocking high-severity)" {
		t.Fatalf("bad detail: %q", res.Detail)
	}

	impGit(t, repo, "checkout", "-b", "devagent/G4-2", "main")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "b.ts"), []byte("const ok = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	impGit(t, repo, "add", "-A")
	impGit(t, repo, "commit", "-m", "clean")
	res2, err := impRunGateG4(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Passed || res2.Detail != "1 changed file(s), 0 finding(s)" {
		t.Fatalf("clean change must pass: %+v", res2)
	}
}

// --- publish stage (real git + fake gh on PATH) ---

func TestImpPublishStage(t *testing.T) {
	repo := impInitGitRepo(t)
	binDir := t.TempDir()
	rec := filepath.Join(t.TempDir(), "gh-args.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\nif [ \"$1\" = \"pr\" ] && [ \"$2\" = \"create\" ]; then echo https://github.com/acme/repo/pull/9; exit 0; fi\nexit 0\n", rec)
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	wt, err := git.CreateWorktree(repo, "P-PUB")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.WorktreePath, "feat.ts"), []byte("const ok = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := &impCaptureLog{}
	deps := BuildDeps(config.Credentials{GithubToken: "tok"}, StageConfig{RepoPath: repo}, log)
	plan := ImplementationPlan{
		Ticket:         scout.TicketSpec{ID: "P-PUB", Title: "Publish test", AcceptanceCriteria: []string{"works"}},
		Classification: "endpoint-only",
		Tasks:          []string{"ship it"},
	}
	prURL, err := deps.PublishStage(RunConfig{RepoPath: repo}, plan, ImplementResult{WorktreePath: wt.WorktreePath})
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	if prURL != "https://github.com/acme/repo/pull/9" {
		t.Fatalf("bad pr url: %q", prURL)
	}
	// The worker's uncommitted output must be committed with the pinned message.
	subject := impGitOut(t, wt.WorktreePath, "log", "-1", "--pretty=%s")
	if subject != "devagent(P-PUB): Publish test" {
		t.Fatalf("bad commit subject: %q", subject)
	}
	// The PR body carries the changed-file evidence and closes the ticket.
	raw, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{"## Files changed", "- `feat.ts`", "Closes P-PUB.", "- [ ] works"} {
		if !strings.Contains(body, want) {
			t.Fatalf("pr body missing %q: %s", want, body)
		}
	}

	// No worktree path short-circuits to "" without touching gh.
	prURL2, err := deps.PublishStage(RunConfig{RepoPath: repo}, plan, ImplementResult{})
	if err != nil || prURL2 != "" {
		t.Fatalf("empty worktree must publish nothing: %q %v", prURL2, err)
	}
}

// --- BuildDeps ticket routing + dry-run ---

func TestImpBuildDepsFetchTicketGitHubNoToken(t *testing.T) {
	deps := BuildDeps(config.Credentials{}, StageConfig{RepoPath: t.TempDir()}, &impCaptureLog{})
	_, err := deps.FetchTicket("acme/repo#3")
	if err == nil || err.Error() != "GitHub issue ref acme/repo#3 requires GITHUB_TOKEN to be set" {
		t.Fatalf("bad error: %v", err)
	}
}

func TestImpBuildDryRunDeps(t *testing.T) {
	deps := BuildDryRunDeps()
	ticket, err := deps.FetchTicket("whatever")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.ID != "DRY-RUN" || ticket.Title != "[dry-run] DRY-RUN" || ticket.Description != "Synthetic ticket for plan-only execution; no tracker credentials required." {
		t.Fatalf("bad dry-run ticket: %+v", ticket)
	}
	check := deps.RunGateG3("/unused", "endpoint-only")
	if !check.Passed || check.Detail != "skipped: dry-run" || len(check.Findings) != 0 {
		t.Fatalf("bad dry-run G3: %+v", check)
	}
}

// --- PR body byte parity ---

func TestImpBuildPrBody(t *testing.T) {
	plan := ImplementationPlan{
		Ticket: scout.TicketSpec{
			ID:                 "LIN-204",
			URL:                "https://linear.app/x/LIN-204",
			AcceptanceCriteria: []string{"a", "b"},
		},
		Classification: "endpoint-only",
		Tasks:          []string{"t1", "t2"},
	}
	got := impBuildPrBody(plan, []string{"src/a.ts"})
	want := strings.Join([]string{
		"Closes LIN-204 (https://linear.app/x/LIN-204).",
		"",
		"## Summary",
		"Automated implementation classified as **endpoint-only** by DevAgent.",
		"",
		"## Plan",
		"1. t1",
		"2. t2",
		"",
		"## Files changed",
		"- `src/a.ts`",
		"",
		"## Validation",
		"- G3 static migration analysis: passed",
		"- Test suite: see CI run on this branch",
		"",
		"## Acceptance criteria",
		"- [ ] a",
		"- [ ] b",
	}, "\n")
	if got != want {
		t.Fatalf("pr body mismatch:\n got: %q\nwant: %q", got, want)
	}
	// No URL, no changed files, no acceptance criteria.
	got = impBuildPrBody(ImplementationPlan{
		Ticket:         scout.TicketSpec{ID: "T-1"},
		Classification: "migration-required",
		Tasks:          nil,
	}, nil)
	want = strings.Join([]string{
		"Closes T-1.",
		"",
		"## Summary",
		"Automated implementation classified as **migration-required** by DevAgent.",
		"",
		"## Plan",
		"",
		"## Validation",
		"- G3 static migration analysis: passed",
		"- Test suite: see CI run on this branch",
		"",
		"## Acceptance criteria",
		"- (see ticket)",
	}, "\n")
	if got != want {
		t.Fatalf("pr body mismatch (minimal):\n got: %q\nwant: %q", got, want)
	}
}

// --- drop-orca no-op ---

func TestImpDropOrcaWorkspaceIfRequestedNoop(t *testing.T) {
	log := &impCaptureLog{}
	// Not requested: nothing happens even on a plain dir.
	DropOrcaWorkspaceIfRequested(StageConfig{RepoPath: t.TempDir()}, log)
	if len(log.Entries) != 0 {
		t.Fatalf("unexpected entries: %+v", log.Entries)
	}
	// Requested on a non-Orca repo: findOrcaWorktreeByPath returns "" → no-op.
	repo := t.TempDir()
	DropOrcaWorkspaceIfRequested(StageConfig{RepoPath: repo, DropOrcaWorkspace: true}, log)
	if log.has("info", "Orca workspace dropped") {
		t.Fatalf("plain repo must not drop anything: %+v", log.Entries)
	}
}
