package pipeline

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/lessons"
	"github.com/FreePeak/devagent/internal/orchestrator"
	"github.com/FreePeak/devagent/internal/queue"
	"github.com/FreePeak/devagent/internal/spawn"
)

// ---------------------------------------------------------------------------
// Helpers (cns = consume unit).

type cnsLogEntry struct {
	Level   string
	Stage   ledger.RunStage
	Message string
	Data    []ledger.KV
}

// cnsRecorder is the injected no-op-ish RunLog: it records instead of
// writing a run-log file, so consume tests stay hermetic and can pin the
// exact warn/error lines (including the fenced-write refusals).
type cnsRecorder struct {
	entries []cnsLogEntry
}

func (r *cnsRecorder) Info(stage ledger.RunStage, message string, data []ledger.KV) {
	r.entries = append(r.entries, cnsLogEntry{Level: "info", Stage: stage, Message: message, Data: data})
}

func (r *cnsRecorder) Warn(stage ledger.RunStage, message string, data []ledger.KV) {
	r.entries = append(r.entries, cnsLogEntry{Level: "warn", Stage: stage, Message: message, Data: data})
}

func (r *cnsRecorder) Error(stage ledger.RunStage, message string, data []ledger.KV) {
	r.entries = append(r.entries, cnsLogEntry{Level: "error", Stage: stage, Message: message, Data: data})
}

func (r *cnsRecorder) has(level, substr string) bool {
	for _, e := range r.entries {
		if e.Level == level && strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}

// cnsGit runs a git command in dir and fails the test on error.
func cnsGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// cnsGitRepo creates a temp git repo on branch `main` with one commit.
// withPkgJson adds the TS fixture's package.json (test = node -e ""), so
// the lessons-guard suite step is green without network.
func cnsGitRepo(t *testing.T, withPkgJson bool) string {
	t.Helper()
	dir := t.TempDir()
	cnsGit(t, dir, "init", "-q", "-b", "main")
	cnsGit(t, dir, "config", "user.email", "t@t")
	cnsGit(t, dir, "config", "user.name", "t")
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "PRD.md"), []byte("# PRD\n## 4 Competitive\nfoo\n## 17 Roadmap\nbar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if withPkgJson {
		pkg := map[string]any{"name": "fixture", "scripts": map[string]string{"test": `node -e ""`}}
		raw, _ := json.Marshal(pkg)
		if err := os.WriteFile(filepath.Join(dir, "package.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cnsGit(t, dir, "add", "-A")
	cnsGit(t, dir, "commit", "-qm", "init")
	return dir
}

// cnsCommitOnBranch writes files on a fresh branch and returns to main.
// Only the branch name and the listed files are staged (never -A: the
// queue fixture must not be swept into the branch commit, or the checkout
// back to main deletes it).
func cnsCommitOnBranch(t *testing.T, repo, branch string, files map[string]string) {
	t.Helper()
	cnsGit(t, repo, "checkout", "-q", "-b", branch)
	names := make([]string, 0, len(files))
	for name, content := range files {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	cnsGit(t, repo, append([]string{"add"}, names...)...)
	cnsGit(t, repo, "commit", "-qm", "change on "+branch)
	cnsGit(t, repo, "checkout", "-q", "main")
}

// cnsFakeDeps returns PipelineDeps that stand in for implstage.BuildDeps:
// the pipeline runs end-to-end with a fake worker (the TS suite mocks
// getWorker the same way).
func cnsFakeDeps(impl ImplementResult, prURL string, g1Detail string, g1Pass bool) PipelineDeps {
	return PipelineDeps{
		ImplementStage: func(RunConfig, ImplementationPlan, RunLog) (ImplementResult, error) {
			return impl, nil
		},
		RunGateG1: func(string, int) (G2G4Result, error) {
			return G2G4Result{Passed: g1Pass, Detail: g1Detail}, nil
		},
		PublishStage: func(RunConfig, ImplementationPlan, ImplementResult) (string, error) {
			return prURL, nil
		},
	}
}

// cnsInstallSeams wires the consume test seams (fake deps, recorder log,
// no-op sleep) and restores everything on cleanup.
func cnsInstallSeams(t *testing.T, deps func(creds config.Credentials, cfg StageConfig, log RunLog) PipelineDeps) *cnsRecorder {
	t.Helper()
	oldDeps, oldLog, oldSleep := cnsBuildDeps, cnsNewRunLog, cnsSleep
	rec := &cnsRecorder{}
	cnsBuildDeps = deps
	cnsNewRunLog = func() RunLog { return rec }
	cnsSleep = func(int) {}
	t.Cleanup(func() {
		cnsBuildDeps, cnsNewRunLog, cnsSleep = oldDeps, oldLog, oldSleep
	})
	return rec
}

func cnsEnqueue(t *testing.T, repo, id, goal, description string) {
	t.Helper()
	if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{
		ID: id, Title: "Queued job", Goal: goal, Description: description,
	}); err != nil {
		t.Fatal(err)
	}
}

func cnsPtr[T any](v T) *T { return &v }

// ---------------------------------------------------------------------------
// consumeOnce: claim / complete / fail / requeue fencing.

func TestCnsConsumeNoPending(t *testing.T) {
	repo := cnsGitRepo(t, false)
	cnsInstallSeams(t, nil)
	res, err := ConsumeOnce(ConsumeOptions{RepoPath: repo, MaxLoops: 1, TimeoutMs: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Detail != "no pending tasks" {
		t.Fatalf("got %+v", res)
	}
}

func TestCnsConsumeClaimCompletesAndPublishes(t *testing.T) {
	repo := cnsGitRepo(t, false)
	goal := "Goal: queued job does a tiny docs edit with enough description"
	cnsEnqueue(t, repo, "Q-1", goal, "Extra acceptance detail")

	rec := cnsInstallSeams(t, func(config.Credentials, StageConfig, RunLog) PipelineDeps {
		d := cnsFakeDeps(ImplementResult{OK: true, Worker: "omp"}, "https://github.com/o/r/pull/17", "", true)
		d.FetchTicket = func(string) (TicketSpec, error) {
			return TicketSpec{
				ID: "Q-1", Title: "Queued job",
				Description: goal + "\n\nExtra acceptance detail",
				Labels:      []string{}, TrackerInternalID: "Q-1",
			}, nil
		}
		return d
	})

	res, err := ConsumeOnce(ConsumeOptions{RepoPath: repo, AutoPr: true, MaxLoops: 1, TimeoutMs: 15_000})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.TaskID != "Q-1" || res.PRURL != "https://github.com/o/r/pull/17" {
		t.Fatalf("got %+v", res)
	}
	if res.Detail != "done: Q-1 -> https://github.com/o/r/pull/17" {
		t.Fatalf("detail = %q", res.Detail)
	}
	task := queue.ReadTask(repo, "Q-1")
	if task == nil || task.Status != queue.StatusDone {
		t.Fatalf("task = %+v", task)
	}
	if !rec.has("info", "Claimed Q-1: Queued job") {
		t.Fatalf("missing claim log: %+v", rec.entries)
	}
}

func TestCnsConsumeFencedCompletionRefused(t *testing.T) {
	repo := cnsGitRepo(t, false)
	cnsEnqueue(t, repo, "Q-1", "Goal: queued job does a tiny docs edit with enough description", "")

	rec := cnsInstallSeams(t, func(config.Credentials, StageConfig, RunLog) PipelineDeps {
		d := cnsFakeDeps(ImplementResult{OK: true, Worker: "omp"}, "https://github.com/o/r/pull/18", "", true)
		d.FetchTicket = func(string) (TicketSpec, error) {
			return TicketSpec{ID: "Q-1", Title: "Queued job", Description: "long enough description here for the spec check", Labels: []string{}}, nil
		}
		// Mid-run lease move: the fencing token bumps, so our completion
		// must be refused.
		d.ImplementStage = func(RunConfig, ImplementationPlan, RunLog) (ImplementResult, error) {
			queue.UpdateTask(repo, "Q-1", &queue.TaskPatch{LeaseGeneration: cnsPtr(99)}, -1, nil)
			return ImplementResult{OK: true, Worker: "omp"}, nil
		}
		return d
	})

	res, err := ConsumeOnce(ConsumeOptions{RepoPath: repo, AutoPr: true, MaxLoops: 1, TimeoutMs: 15_000})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("got %+v", res)
	}
	if !rec.has("warn", "Completion of Q-1 refused — lease moved on (generation 1)") {
		t.Fatalf("refusal warn missing: %+v", rec.entries)
	}
	task := queue.ReadTask(repo, "Q-1")
	if task == nil || task.Status != queue.StatusClaimed {
		t.Fatalf("task should stay claimed: %+v", task)
	}
}

func TestCnsConsumeTransientRequeuesWithSweep(t *testing.T) {
	repo := cnsGitRepo(t, false)
	goal := "Goal: queued job does a tiny docs edit with enough description"
	cnsEnqueue(t, repo, "Q-1", goal, "")

	rec := cnsInstallSeams(t, func(config.Credentials, StageConfig, RunLog) PipelineDeps {
		d := cnsFakeDeps(ImplementResult{OK: true}, "", "worker timed out after 90000ms", false)
		d.FetchTicket = func(string) (TicketSpec, error) {
			return TicketSpec{ID: "Q-1", Title: "Queued job", Description: goal, Labels: []string{}}, nil
		}
		return d
	})

	// Reaper sweep seams: one stale worker whose command carries the task
	// id, one unrelated stale worker, and a recorded sleep for the backoff.
	oldProbes, oldKill := reapProbes, reapKillTree
	var killed []int
	var slept []int
	cnsSleep = func(ms int) { slept = append(slept, ms) }
	reapProbes = struct {
		cmdline func(pid int) string
		ppid    func(pid int) (int, bool)
		cwd     func(pid int) string
		scan    func() (string, bool)
	}{
		scan: func() (string, bool) {
			return "  PID     ELAPSED COMMAND\n" +
				"4242    20:00 omp -p work on Q-1 --mode json\n" +
				"4243    20:00 omp -p unrelated job --mode json\n", true
		},
		ppid: func(int) (int, bool) { return 1, true },
		cwd:  func(int) string { return filepath.Join(repo, ".devagent-worktrees", "w1") },
	}
	reapKillTree = func(pid int) bool { killed = append(killed, pid); return true }
	t.Cleanup(func() { reapProbes, reapKillTree = oldProbes, oldKill })

	res, err := ConsumeOnce(ConsumeOptions{RepoPath: repo, MaxLoops: 1, TimeoutMs: 15_000})
	if err != nil {
		t.Fatal(err)
	}
	want := "test gate failed: worker timed out after 90000ms (transient — requeued)"
	if res.OK || res.Detail != want {
		t.Fatalf("got %+v want detail %q", res, want)
	}
	task := queue.ReadTask(repo, "Q-1")
	if task == nil || task.Status != queue.StatusPending {
		t.Fatalf("task should be requeued pending: %+v", task)
	}
	if task.Attempts == nil || *task.Attempts != 1 {
		t.Fatalf("attempts = %+v", task.Attempts)
	}
	if task.LastError == nil || !strings.Contains(*task.LastError, "timed out") {
		t.Fatalf("lastError = %+v", task.LastError)
	}
	if len(killed) != 1 || killed[0] != 4242 {
		t.Fatalf("sweep killed = %v (want only the task-scoped worker)", killed)
	}
	// The claim already bumped attempts to 1, so the requeue waits on
	// backoffDelay(2) = 4000ms ± 25% jitter.
	if len(slept) != 1 || slept[0] < 3000 || slept[0] > 5000 {
		t.Fatalf("backoff sleep = %v (want ~4000ms for attempt 2)", slept)
	}
	if !rec.has("warn", "Transient infra failure for Q-1, requeued as pending") {
		t.Fatalf("requeue warn missing: %+v", rec.entries)
	}
}

func TestCnsConsumeFencedRequeueRefused(t *testing.T) {
	repo := cnsGitRepo(t, false)
	goal := "Goal: queued job does a tiny docs edit with enough description"
	cnsEnqueue(t, repo, "Q-1", goal, "")

	cnsInstallSeams(t, func(config.Credentials, StageConfig, RunLog) PipelineDeps {
		d := cnsFakeDeps(ImplementResult{OK: true}, "", "watchdog fired: no-progress", false)
		d.FetchTicket = func(string) (TicketSpec, error) {
			return TicketSpec{ID: "Q-1", Title: "Queued job", Description: goal, Labels: []string{}}, nil
		}
		d.ImplementStage = func(RunConfig, ImplementationPlan, RunLog) (ImplementResult, error) {
			queue.UpdateTask(repo, "Q-1", &queue.TaskPatch{LeaseGeneration: cnsPtr(99)}, -1, nil)
			return ImplementResult{OK: true}, nil
		}
		return d
	})

	res, err := ConsumeOnce(ConsumeOptions{RepoPath: repo, MaxLoops: 1, TimeoutMs: 15_000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(res.Detail, "(transient — requeued)") {
		t.Fatalf("detail = %q", res.Detail)
	}
	// The requeue itself was refused (stale token), so the task stays
	// claimed at the bumped generation.
	task := queue.ReadTask(repo, "Q-1")
	if task == nil || task.Status != queue.StatusClaimed {
		t.Fatalf("task = %+v", task)
	}
}

func TestCnsConsumeBoundedFailureGoesToFailed(t *testing.T) {
	repo := cnsGitRepo(t, false)
	goal := "Goal: queued job does a tiny docs edit with enough description"
	cnsEnqueue(t, repo, "Q-1", goal, "")
	raw, _ := json.Marshal(map[string]any{"resilience": map[string]any{"apiMaxAttempts": 3}})
	if err := os.WriteFile(filepath.Join(repo, "devagent.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	cnsInstallSeams(t, func(config.Credentials, StageConfig, RunLog) PipelineDeps {
		d := cnsFakeDeps(ImplementResult{OK: true}, "", "worker timed out after 90000ms", false)
		d.FetchTicket = func(string) (TicketSpec, error) {
			return TicketSpec{ID: "Q-1", Title: "Queued job", Description: goal, Labels: []string{}}, nil
		}
		return d
	})

	res, err := ConsumeOnce(ConsumeOptions{RepoPath: repo, MaxLoops: 1, TimeoutMs: 15_000})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Detail, "requeued") {
		t.Fatalf("bounded retries must not requeue: %q", res.Detail)
	}
	task := queue.ReadTask(repo, "Q-1")
	if task == nil || task.Status != queue.StatusFailed {
		t.Fatalf("task = %+v", task)
	}
}

func TestCnsConsumeCrashTransientRequeued(t *testing.T) {
	repo := cnsGitRepo(t, false)
	cnsEnqueue(t, repo, "Q-1", "Goal: queued job does a tiny docs edit with enough description", "")

	cnsInstallSeams(t, func(config.Credentials, StageConfig, RunLog) PipelineDeps {
		// The queued ticket always overrides fetchTicket (consume.ts:420),
		// so the crash path is exercised through a gate error instead.
		return PipelineDeps{
			ImplementStage: func(RunConfig, ImplementationPlan, RunLog) (ImplementResult, error) {
				return ImplementResult{OK: true, Worker: "omp"}, nil
			},
			RunGateG1: func(string, int) (G2G4Result, error) {
				return G2G4Result{}, errors.New("upstream request failed: 503 service unavailable")
			},
		}
	})

	res, err := ConsumeOnce(ConsumeOptions{RepoPath: repo, MaxLoops: 1, TimeoutMs: 15_000})
	if err != nil {
		t.Fatal(err)
	}
	want := "crashed: upstream request failed: 503 service unavailable (transient — requeued)"
	if res.Detail != want {
		t.Fatalf("detail = %q want %q", res.Detail, want)
	}
	task := queue.ReadTask(repo, "Q-1")
	if task == nil || task.Status != queue.StatusPending {
		t.Fatalf("task = %+v", task)
	}
}

func TestCnsConsumeCrashNonTransientFails(t *testing.T) {
	repo := cnsGitRepo(t, false)
	cnsEnqueue(t, repo, "Q-1", "Goal: queued job does a tiny docs edit with enough description", "")

	rec := cnsInstallSeams(t, func(config.Credentials, StageConfig, RunLog) PipelineDeps {
		return PipelineDeps{
			ImplementStage: func(RunConfig, ImplementationPlan, RunLog) (ImplementResult, error) {
				return ImplementResult{OK: true, Worker: "omp"}, nil
			},
			RunGateG1: func(string, int) (G2G4Result, error) {
				return G2G4Result{}, errors.New("TypeError: cannot read properties of undefined")
			},
		}
	})

	res, err := ConsumeOnce(ConsumeOptions{RepoPath: repo, MaxLoops: 1, TimeoutMs: 15_000})
	if err != nil {
		t.Fatal(err)
	}
	if res.Detail != "crashed: TypeError: cannot read properties of undefined" {
		t.Fatalf("detail = %q", res.Detail)
	}
	task := queue.ReadTask(repo, "Q-1")
	if task == nil || task.Status != queue.StatusFailed {
		t.Fatalf("task = %+v", task)
	}
	if !rec.has("error", "Task Q-1 crashed:") {
		t.Fatalf("crash error log missing: %+v", rec.entries)
	}
}

// ---------------------------------------------------------------------------
// Auto-merge ladder: pinned detail strings.

func TestCnsConsumeAutoMergeStrideBlocked(t *testing.T) {
	repo := cnsGitRepo(t, false)
	cnsEnqueue(t, repo, "Q-1", "Goal: queued job does a tiny docs edit with enough description", "")
	cnsCommitOnBranch(t, repo, "devagent/q-1", map[string]string{
		"config.py": "api_key = \"supersecretvalue123456\"\n",
	})

	cnsInstallSeams(t, func(config.Credentials, StageConfig, RunLog) PipelineDeps {
		d := cnsFakeDeps(ImplementResult{OK: true, Branch: "devagent/q-1"}, "https://github.com/o/r/pull/21", "", true)
		d.FetchTicket = func(string) (TicketSpec, error) {
			return TicketSpec{ID: "Q-1", Title: "Queued job", Description: "Goal: queued job does a tiny docs edit with enough description", Labels: []string{}}, nil
		}
		return d
	})

	res, err := ConsumeOnce(ConsumeOptions{RepoPath: repo, AutoPr: true, AutoMerge: true, MaxLoops: 1, TimeoutMs: 15_000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Detail, "done: Q-1 -> https://github.com/o/r/pull/21 (stride gate blocked merge: CRITICAL findings: ") ||
		!strings.HasSuffix(res.Detail, ")") {
		t.Fatalf("detail = %q", res.Detail)
	}
	if res.Merged {
		t.Fatal("merged must be false")
	}
	task := queue.ReadTask(repo, "Q-1")
	if task == nil || task.Status != queue.StatusDone {
		t.Fatalf("task = %+v", task)
	}
}

func TestCnsConsumeAutoMergeMergedResultConflict(t *testing.T) {
	repo := cnsGitRepo(t, false)
	cnsEnqueue(t, repo, "Q-1", "Goal: queued job does a tiny docs edit with enough description", "")
	cnsCommitOnBranch(t, repo, "feat-a", map[string]string{"docs/PRD.md": "# PRD\n## 4 Competitive\nA line\n## 17 Roadmap\nbar\n"})
	cnsCommitOnBranch(t, repo, "feat-b", map[string]string{"docs/PRD.md": "# PRD\n## 4 Competitive\nB line\n## 17 Roadmap\nbar\n"})

	oldList := defaultListOpenPrs
	defaultListOpenPrs = func(string) ([]orchestrator.PrStatus, error) {
		return []orchestrator.PrStatus{
			{HeadRefName: "feat-a", BaseRefName: "main"},
			{HeadRefName: "feat-b", BaseRefName: "main"},
		}, nil
	}
	t.Cleanup(func() { defaultListOpenPrs = oldList })

	cnsInstallSeams(t, func(config.Credentials, StageConfig, RunLog) PipelineDeps {
		d := cnsFakeDeps(ImplementResult{OK: true, Branch: "feat-a"}, "https://github.com/o/r/pull/22", "", true)
		d.FetchTicket = func(string) (TicketSpec, error) {
			return TicketSpec{ID: "Q-1", Title: "Queued job", Description: "Goal: queued job does a tiny docs edit with enough description", Labels: []string{}}, nil
		}
		return d
	})

	res, err := ConsumeOnce(ConsumeOptions{RepoPath: repo, AutoPr: true, AutoMerge: true, MaxLoops: 1, TimeoutMs: 15_000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Detail, "done: Q-1 -> https://github.com/o/r/pull/22 (merge-conflict: ") {
		t.Fatalf("detail = %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "CONFLICT") {
		t.Fatalf("excerpt should carry the git conflict output: %q", res.Detail)
	}
	if res.Merged {
		t.Fatal("conflict must not merge")
	}
	task := queue.ReadTask(repo, "Q-1")
	if task == nil || task.Status != queue.StatusDone {
		t.Fatalf("task = %+v", task)
	}
}

func TestCnsConsumeAutoMergedRecordsKgEvidence(t *testing.T) {
	// package.json so the guard's suite step is green without network.
	repo := cnsGitRepo(t, true)
	cnsEnqueue(t, repo, "Q-1", "Goal: queued job does a tiny docs edit with enough description", "")
	cnsCommitOnBranch(t, repo, "feat-clean", map[string]string{"docs/notes.md": "clean change\n"})

	oldList := defaultListOpenPrs
	defaultListOpenPrs = func(string) ([]orchestrator.PrStatus, error) {
		return []orchestrator.PrStatus{{HeadRefName: "feat-clean", BaseRefName: "main"}}, nil
	}
	oldMerge := cnsAutoMergePr
	cnsAutoMergePr = func(string, string) error { return nil }
	t.Cleanup(func() { defaultListOpenPrs, cnsAutoMergePr = oldList, oldMerge })

	ev := &KgEvidence{Excerpt: "retrieval: L1 (exact) | freshness: fresh", Freshness: cnsPtr("fresh")}
	cnsInstallSeams(t, func(config.Credentials, StageConfig, RunLog) PipelineDeps {
		d := cnsFakeDeps(ImplementResult{OK: true, Branch: "feat-clean", KgEvidence: ev}, "https://github.com/o/r/pull/23", "", true)
		d.FetchTicket = func(string) (TicketSpec, error) {
			return TicketSpec{ID: "Q-1", Title: "Queued job", Description: "Goal: queued job does a tiny docs edit with enough description", Labels: []string{}}, nil
		}
		return d
	})

	res, err := ConsumeOnce(ConsumeOptions{RepoPath: repo, AutoPr: true, AutoMerge: true, MaxLoops: 1, TimeoutMs: 60_000})
	if err != nil {
		t.Fatal(err)
	}
	if res.Detail != "done: Q-1 -> https://github.com/o/r/pull/23 (auto-merged)" {
		t.Fatalf("detail = %q", res.Detail)
	}
	if !res.Merged {
		t.Fatal("merged must be true")
	}
	raw, readErr := os.ReadFile(filepath.Join(repo, lessons.LessonsPath))
	if readErr != nil {
		t.Fatalf("lessons file: %v", readErr)
	}
	if !strings.Contains(string(raw), "KG digest evidence persisted on merge: retrieval: L1 (exact) | freshness: fresh") {
		t.Fatalf("lessons file missing the verbatim KG line:\n%s", raw)
	}
}

// ---------------------------------------------------------------------------
// Merged-result oracle (direct).

func TestCnsMergedResultOracleDisabled(t *testing.T) {
	repo := cnsGitRepo(t, false)
	res, err := RunMergedResultOracle(repo, "main", MergedResultOracleOptions{TimeoutMs: 1000, Enabled: cnsPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed || !res.Skipped || res.Reason != "disabled" {
		t.Fatalf("got %+v", res)
	}
}

func TestCnsMergedResultOracleEnumerationFallback(t *testing.T) {
	repo := cnsGitRepo(t, false)
	cnsCommitOnBranch(t, repo, "feat-solo", map[string]string{"docs/solo.md": "solo\n"})
	res, err := RunMergedResultOracle(repo, "feat-solo", MergedResultOracleOptions{
		TimeoutMs: 10_000,
		ListPrs:   func() ([]orchestrator.PrStatus, error) { return nil, errors.New("gh pr list exited 1") },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Candidate-only board on main; the fixture repo has no runnable test
	// command, so the gate skips open (auto-merge proceeds).
	if !res.Passed || !res.Skipped || res.Reason != "no-test-command" {
		t.Fatalf("got %+v", res)
	}
}

func TestCnsMergedResultOracleConflictBlocks(t *testing.T) {
	repo := cnsGitRepo(t, false)
	// A bare origin mirrors the real layout (the oracle resolves `main`).
	origin := filepath.Join(t.TempDir(), "origin.git")
	cnsGit(t, filepath.Dir(origin), "init", "-q", "--bare", origin)
	cnsGit(t, repo, "remote", "add", "origin", origin)
	cnsGit(t, repo, "push", "-q", "origin", "main")
	cnsCommitOnBranch(t, repo, "feat-a", map[string]string{"docs/PRD.md": "# PRD\n## 4 Competitive\nA\n## 17 Roadmap\nbar\n"})
	cnsCommitOnBranch(t, repo, "feat-b", map[string]string{"docs/PRD.md": "# PRD\n## 4 Competitive\nB\n## 17 Roadmap\nbar\n"})
	cnsGit(t, repo, "push", "-q", "origin", "feat-a", "feat-b")

	res, err := RunMergedResultOracle(repo, "feat-a", MergedResultOracleOptions{
		TimeoutMs: 10_000,
		ListPrs: func() ([]orchestrator.PrStatus, error) {
			return []orchestrator.PrStatus{
				{HeadRefName: "feat-a", BaseRefName: "main"},
				{HeadRefName: "feat-b", BaseRefName: "main"},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || res.Skipped || res.Reason != "merge-conflict" {
		t.Fatalf("got %+v", res)
	}
	if !strings.Contains(res.Excerpt, "CONFLICT") {
		t.Fatalf("excerpt = %q", res.Excerpt)
	}
	// The staging worktree must always be removed.
	entries, _ := os.ReadDir(filepath.Join(repo, ".devagent-worktrees"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "merged-") {
			t.Fatalf("staging worktree left behind: %s", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// Pure classifiers.

func TestCnsIsConsumeTransient(t *testing.T) {
	cases := []struct {
		detail string
		want   bool
	}{
		{"worker timed out after 90000ms", true},
		{"TiMeD-Out", true},
		{"watchdog fired", true},
		{"no-progress threshold hit", true},
		{"429 too many requests", true},
		{"test gate failed: assertion mismatch", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsConsumeTransient(c.detail); got != c.want {
			t.Errorf("IsConsumeTransient(%q) = %v want %v", c.detail, got, c.want)
		}
	}
}

func TestCnsParsePrBranch(t *testing.T) {
	if got := parsePrBranch("https://github.com/o/r/pull/123"); got != "devagent/pull-123" {
		t.Errorf("got %q", got)
	}
	if got := parsePrBranch("https://gitlab.com/o/r/-/merge_requests/9"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestCnsRedactSecrets(t *testing.T) {
	got := cnsRedactSecrets("clone https://user:hunter2tok@github.com/o/r failed")
	if got != "clone https://<redacted>@github.com/o/r failed" {
		t.Errorf("got %q", got)
	}
	got = cnsRedactSecrets("token ghp_abcdefghijklmnop1234 rejected")
	if !strings.Contains(got, "<redacted>") || strings.Contains(got, "ghp_abcdefghijklmnop") {
		t.Errorf("got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Self-update hook.

func TestCnsSelfUpdateDirtyWorktreeSkips(t *testing.T) {
	oldRunner := cnsSelfUpdateRunner
	cnsSelfUpdateRunner = func(cmd string, args []string, opts spawn.Options) spawn.Result {
		if cmd == "git" && args[0] == "status" {
			return spawn.Result{ExitCode: 0, Stdout: " M src/app.ts\n"}
		}
		return spawn.Result{ExitCode: 0}
	}
	t.Cleanup(func() { cnsSelfUpdateRunner = oldRunner })

	err := cnsRunSelfUpdate("/tmp/ignored", &cnsRecorder{})
	if err == nil || err.Error() != "self-update skipped: dirty worktree (1 file(s))" {
		t.Fatalf("err = %v", err)
	}
}

func TestCnsSelfUpdateGreenPath(t *testing.T) {
	oldRunner := cnsSelfUpdateRunner
	var calls []string
	cnsSelfUpdateRunner = func(cmd string, args []string, opts spawn.Options) spawn.Result {
		calls = append(calls, cmd+" "+strings.Join(args, " "))
		return spawn.Result{ExitCode: 0}
	}
	t.Cleanup(func() { cnsSelfUpdateRunner = oldRunner })

	rec := &cnsRecorder{}
	if err := cnsRunSelfUpdate("/tmp/ignored", rec); err != nil {
		t.Fatal(err)
	}
	if len(calls) < 4 || !strings.HasPrefix(calls[0], "git status --porcelain") ||
		!strings.HasPrefix(calls[1], "git pull --ff-only") {
		t.Fatalf("calls = %v", calls)
	}
	found := false
	for _, e := range rec.entries {
		if e.Level == "info" && strings.HasPrefix(e.Message, "self-update ok: pull -> install -> build") {
			found = true
		}
	}
	if !found {
		t.Fatalf("entries = %+v (want self-update ok detail)", rec.entries)
	}
}

// ---------------------------------------------------------------------------
// recordMergedKgEvidence.

func TestCnsRecordMergedKgEvidenceFreshnessGate(t *testing.T) {
	repo := cnsGitRepo(t, true)

	// Stale evidence: nothing is persisted.
	stale := &KgEvidence{Excerpt: "retrieval: L0 (cold) | freshness: stale", Freshness: cnsPtr("stale")}
	res, err := RecordMergedKgEvidence(repo, stale, nil)
	if err != nil || res != nil {
		t.Fatalf("stale evidence must be omitted: %+v %v", res, err)
	}
	if _, statErr := os.Stat(filepath.Join(repo, lessons.LessonsPath)); statErr == nil {
		t.Fatal("lessons file must not be created for stale evidence")
	}

	fresh := &KgEvidence{Excerpt: "retrieval: L2 (fuzzy) | freshness: fresh", Freshness: cnsPtr("fresh")}
	res, err = RecordMergedKgEvidence(repo, fresh, &KgEvidenceRecordOptions{Log: &cnsRecorder{}})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.Reason != lessons.ReasonAccepted {
		t.Fatalf("res = %+v", res)
	}
	raw, readErr := os.ReadFile(filepath.Join(repo, lessons.LessonsPath))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(raw), "KG digest evidence persisted on merge: retrieval: L2 (fuzzy) | freshness: fresh") {
		t.Fatalf("lessons file:\n%s", raw)
	}
	if !strings.Contains(string(raw), kgEvidencePredictedImpact) {
		t.Fatalf("predictedImpact must be recorded:\n%s", raw)
	}
}

// ---------------------------------------------------------------------------
// LeanKG client.

func cnsFakeBin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kg-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCnsQueryLeanKgDegradedModes(t *testing.T) {
	repo := t.TempDir()

	// missing-binary
	res := QueryLeanKg(LeanKgCallOptions{RepoPath: repo, Query: "q", Bin: "/nonexistent/leankg-missing"})
	if res.Degraded == nil || res.Degraded.Reason != "missing-binary" ||
		res.Degraded.Detail != "/nonexistent/leankg-missing binary not found" {
		t.Fatalf("missing-binary: %+v", res.Degraded)
	}

	// timeout
	slow := cnsFakeBin(t, "sleep 5")
	res = QueryLeanKg(LeanKgCallOptions{RepoPath: repo, Query: "q", Bin: slow, TimeoutMs: 150})
	if res.Degraded == nil || res.Degraded.Reason != "timeout" ||
		!strings.HasSuffix(res.Degraded.Detail, "query exceeded 150ms budget (killed)") {
		t.Fatalf("timeout: %+v", res.Degraded)
	}

	// exit
	bad := cnsFakeBin(t, "echo boom >&2\nexit 3")
	res = QueryLeanKg(LeanKgCallOptions{RepoPath: repo, Query: "q", Bin: bad, TimeoutMs: 10000})
	if res.Degraded == nil || res.Degraded.Reason != "exit" ||
		!strings.HasSuffix(res.Degraded.Detail, "query exited 3: boom") {
		t.Fatalf("exit: %+v", res.Degraded)
	}

	// empty output
	empty := cnsFakeBin(t, "true")
	res = QueryLeanKg(LeanKgCallOptions{RepoPath: repo, Query: "q", Bin: empty, TimeoutMs: 10000})
	if res.Degraded == nil || res.Degraded.Reason != "parse" ||
		!strings.HasSuffix(res.Degraded.Detail, "returned empty output") {
		t.Fatalf("empty: %+v", res.Degraded)
	}

	// invalid JSON
	junk := cnsFakeBin(t, "echo 'not json'")
	res = QueryLeanKg(LeanKgCallOptions{RepoPath: repo, Query: "q", Bin: junk, TimeoutMs: 10000})
	if res.Degraded == nil || res.Degraded.Reason != "parse" ||
		!strings.HasSuffix(res.Degraded.Detail, "response is not valid JSON") {
		t.Fatalf("junk: %+v", res.Degraded)
	}
}

func TestCnsQueryLeanKgDigestAndProvenance(t *testing.T) {
	repo := t.TempDir()
	ok := cnsFakeBin(t, `printf '%s' '{"answer":"line one\nline two","retrieval":{"rung":"L3","reason":"vectors"},"freshness":"fresh"}'`)
	res := QueryLeanKg(LeanKgCallOptions{RepoPath: repo, Query: "q", Bin: ok, TimeoutMs: 10000})
	if res.Degraded != nil {
		t.Fatalf("degraded: %+v", res.Degraded)
	}
	want := "line one\nline two\nretrieval: L3 (vectors) | freshness: fresh"
	if res.Content != want {
		t.Fatalf("content = %q want %q", res.Content, want)
	}
	if res.Provenance == nil || *res.Provenance.Rung != "L3" || *res.Provenance.Freshness != "fresh" {
		t.Fatalf("provenance = %+v", res.Provenance)
	}

	// results[] shape + numeric rung (String() rendering).
	multi := cnsFakeBin(t, `printf '%s' '{"results":[{"name":"a","description":"d1"},{"title":"t"},{"other":1}],"retrieval":{"rung":3},"freshness":"possibly_stale"}'`)
	res = QueryLeanKg(LeanKgCallOptions{RepoPath: repo, Query: "q", Bin: multi, TimeoutMs: 10000})
	want = "a: d1\nt\n{\"other\":1}\nretrieval: 3 | freshness: possibly_stale"
	if res.Content != want {
		t.Fatalf("content = %q want %q", res.Content, want)
	}
}

func TestCnsLeanKgProviderSurfacingAndEvidence(t *testing.T) {
	repo := t.TempDir()
	rec := &cnsRecorder{}
	ok := cnsFakeBin(t, `printf '%s' '{"answer":"structural fact","retrieval":{"rung":"L1","reason":"exact"},"freshness":"fresh"}'`)
	provider := CreateLeanKgProvider(LeanKgClientOptions{
		LeanKgCallOptions: LeanKgCallOptions{RepoPath: repo, Query: "q", Bin: ok, TimeoutMs: 10000},
		Log:               rec,
	})
	content := provider.Fn()
	if content != "structural fact\nretrieval: L1 (exact) | freshness: fresh" {
		t.Fatalf("content = %q", content)
	}
	if !rec.has("info", "kg=leankg digest attached: retrieval: L1 (exact) | freshness: fresh") {
		t.Fatalf("info line missing: %+v", rec.entries)
	}
	ev := CaptureKgEvidence(provider)
	if ev == nil || !IsFreshKgEvidence(ev) || ev.Excerpt != "retrieval: L1 (exact) | freshness: fresh" {
		t.Fatalf("evidence = %+v", ev)
	}

	// Degraded provider: warn line, empty digest, no evidence.
	rec2 := &cnsRecorder{}
	bad := cnsFakeBin(t, "exit 7")
	p2 := CreateLeanKgProvider(LeanKgClientOptions{
		LeanKgCallOptions: LeanKgCallOptions{RepoPath: repo, Query: "q", Bin: bad, TimeoutMs: 10000},
		Log:               rec2,
	})
	if got := p2.Fn(); got != "" {
		t.Fatalf("degraded content = %q", got)
	}
	if !rec2.has("warn", "kg=leankg degraded (exit)") || !strings.Contains(rec2.entries[len(rec2.entries)-1].Message, "KG layer omitted from digest") {
		t.Fatalf("warn line missing: %+v", rec2.entries)
	}
	if CaptureKgEvidence(p2) != nil {
		t.Fatal("degraded run must capture no evidence")
	}
}

func TestCnsProvenanceLine(t *testing.T) {
	str := func(s string) *string { return &s }
	if got := ProvenanceLine(LeanKgProvenance{}); got != "" {
		t.Errorf("empty = %q", got)
	}
	if got := ProvenanceLine(LeanKgProvenance{Reason: str("vectors")}); got != "retrieval: unknown (vectors)" {
		t.Errorf("reason-only = %q", got)
	}
	if got := ProvenanceLine(LeanKgProvenance{Freshness: str("cold")}); got != "freshness: cold" {
		t.Errorf("freshness-only = %q", got)
	}
	if got := ProvenanceLine(LeanKgProvenance{Rung: str("L3"), Reason: str("vectors"), Freshness: str("fresh")}); got != "retrieval: L3 (vectors) | freshness: fresh" {
		t.Errorf("full = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Reaper.

func TestCnsParseEtimeToMs(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"45", 45_000},
		{"01:30", 90_000},
		{"1:02:03", 3723_000},
		{"2-03:04:05", 183845_000},
		{" 12:34 ", 754_000},
		{"nonsense", 0},
	}
	for _, c := range cases {
		if got := ParseEtimeToMs(c.in); got != c.want {
			t.Errorf("ParseEtimeToMs(%q) = %d want %d", c.in, got, c.want)
		}
	}
}

func TestCnsIsDevagentWorkerCmd(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"claude -p do the thing --output-format json", true},
		{"opencode --print x --output-format json", true},
		{"omp -p task --mode json", true},
		{"omp --mode=json -p task", true},
		{"pi -c resume --mode json", true},
		{"claude -p no output format", false},
		{"omp", false},
		{"omp --mode json", false}, // interactive: no headless flag
		{"vim notes", false},
	}
	for _, c := range cases {
		if got := IsDevagentWorkerCmd(c.cmd); got != c.want {
			t.Errorf("IsDevagentWorkerCmd(%q) = %v want %v", c.cmd, got, c.want)
		}
	}
}

func cnsReapFakeProbes(t *testing.T, scanOut string, cwd string) *[]int {
	t.Helper()
	oldProbes, oldKill := reapProbes, reapKillTree
	killed := []int{}
	reapProbes = struct {
		cmdline func(pid int) string
		ppid    func(pid int) (int, bool)
		cwd     func(pid int) string
		scan    func() (string, bool)
	}{
		scan:    func() (string, bool) { return scanOut, true },
		ppid:    func(int) (int, bool) { return 1, true },
		cwd:     func(int) string { return cwd },
		cmdline: func(int) string { return "omp -p job --mode json" },
	}
	reapKillTree = func(pid int) bool { killed = append(killed, pid); return true }
	t.Cleanup(func() { reapProbes, reapKillTree = oldProbes, oldKill })
	return &killed
}

func TestCnsFindStaleWorkerPids(t *testing.T) {
	scan := "  PID     ELAPSED COMMAND\n" +
		"4242    20:00 omp -p stale job --mode json\n" +
		"4243    00:30 omp -p fresh job --mode json\n" +
		"4244    20:00 vim notes\n" +
		"4245    20:00 omp --mode json\n"
	killed := cnsReapFakeProbes(t, scan, "/repo/.devagent-worktrees/w1")

	stale := FindStaleWorkerPids(10*60_000, &ReapOptions{CWDPrefix: "/repo/.devagent-worktrees"})
	if len(stale) != 1 || stale[0].Pid != 4242 || stale[0].ElapsedMs != 1200_000 {
		t.Fatalf("stale = %+v", stale)
	}
	if !strings.Contains(stale[0].Command, "stale job") {
		t.Fatalf("command = %q", stale[0].Command)
	}

	// A foreign cwd prefix filters everything out.
	if got := FindStaleWorkerPids(10*60_000, &ReapOptions{CWDPrefix: "/other"}); len(got) != 0 {
		t.Fatalf("cwd filter failed: %+v", got)
	}

	// dry-run reports without killing.
	if got := ReapStaleWorkers(10*60_000, true, nil); len(got) != 1 || len(*killed) != 0 {
		t.Fatalf("dry run = %+v killed = %v", got, *killed)
	}
	if got := ReapStaleWorkers(10*60_000, false, nil); len(got) != 1 || len(*killed) != 1 || (*killed)[0] != 4242 {
		t.Fatalf("reap = %+v killed = %v", got, *killed)
	}
}

func TestCnsKillStaleProcessTreeRefusesNonWorkers(t *testing.T) {
	oldProbes := reapProbes
	reapProbes = struct {
		cmdline func(pid int) string
		ppid    func(pid int) (int, bool)
		cwd     func(pid int) string
		scan    func() (string, bool)
	}{
		cmdline: func(int) string { return "vim my-live-session" },
		ppid:    func(int) (int, bool) { return 1, true },
	}
	t.Cleanup(func() { reapProbes = oldProbes })

	if KillStaleProcessTree(4321) {
		t.Fatal("interactive process must never be reap-eligible")
	}
	if KillStaleProcessTree(1) {
		t.Fatal("pid 1 must never be killed")
	}
}

func TestCnsOwnAncestryPidsWalksParents(t *testing.T) {
	oldProbes := reapProbes
	reapProbes = struct {
		cmdline func(pid int) string
		ppid    func(pid int) (int, bool)
		cwd     func(pid int) string
		scan    func() (string, bool)
	}{
		ppid: func(pid int) (int, bool) {
			switch pid {
			case os.Getpid():
				return 424_242, true
			case 424_242:
				return 424_243, true
			case 424_243:
				return 1, true
			}
			return 0, false
		},
	}
	t.Cleanup(func() { reapProbes = oldProbes })

	own := OwnAncestryPids()
	if !own[os.Getpid()] || !own[424_242] || !own[424_243] || len(own) != 3 {
		t.Fatalf("own = %v", own)
	}
}

func TestCnsConsumeResultJSONTags(t *testing.T) {
	raw, err := json.Marshal(ConsumeResult{OK: true, TaskID: "Q-1", Detail: "d", PRURL: "u", Merged: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"ok":true`, `"taskId":"Q-1"`, `"detail":"d"`, `"prUrl":"u"`, `"merged":true`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("raw = %s (missing %s)", raw, key)
		}
	}
}
