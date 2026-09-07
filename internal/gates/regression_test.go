package gates

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/spawn"
)

// gitRepo builds a real git repo with one commit so `git worktree add` can
// materialize branches (TS gitRepo fixture).
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	// The default fixture always carries package.json (TS gitRepo shape);
	// extra files layer on top without removing it.
	all := map[string]string{
		"package.json": `{"name": "fixture", "scripts": {"test": "node -e \"\""}}`,
	}
	for k, v := range files {
		all[k] = v
	}
	files = all
	writeRepoFiles(t, dir, files)
	git("add", "-A")
	git("commit", "-qm", "init")
	git("branch", "devagent/pull-1")
	return dir
}

func writeRepoFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// dropPackageJson drops package.json and re-points the PR branch at the
// non-JS tree (TS helper).
func dropPackageJson(t *testing.T, repo string) {
	t.Helper()
	if err := os.Remove(filepath.Join(repo, "package.json")); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("add", "-A")
	git("commit", "-qm", "non-js")
	git("branch", "-f", "devagent/pull-1")
}

// realGitWorktreeRunner scripts the suite run while running the real git
// recordingWorktreeRunner runs the real git plumbing AND records calls, so
// lifecycle tests can assert setup/teardown ordering.
type recordingGitRunner struct {
	runner Runner
	calls  []string
}

func (r *recordingGitRunner) RunCli(name string, args []string, opts spawn.Options) spawn.Result {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return r.runner.RunCli(name, args, opts)
}

func TestRunRegressionOraclePassesAndRemovesWorktree(t *testing.T) {
	repo := gitRepo(t, nil)
	sut := &recordingGitRunner{runner: &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		if name == "git" {
			// Run real git worktree plumbing from the repo dir.
			cmd := exec.Command(name, args...)
			cmd.Dir = opts.Dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return spawn.Result{ExitCode: 1, Stderr: string(out)}
			}
			return spawn.Result{ExitCode: 0}
		}
		return spawn.Result{ExitCode: 0}
	}}}
	en := true
	r, err := RunRegressionOracle(repo, "devagent/pull-1", RegressionOracleOptions{TimeoutMs: 10_000, Enabled: &en, Runner: sut, Worktrees: gitWorktreeVia(sut)})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Passed || r.Skipped {
		t.Errorf("expected green run, got %+v err=%v", r, err)
	}
	if r.Excerpt != "" {
		t.Errorf("excerpt should be empty, got %q", r.Excerpt)
	}
	// worktree removed in the finally path: last git call is worktree remove --force
	var last string
	for i := len(sut.calls) - 1; i >= 0; i-- {
		if strings.HasPrefix(sut.calls[i], "git worktree") {
			last = sut.calls[i]
			break
		}
	}
	if !strings.Contains(last, "worktree remove --force") {
		t.Errorf("expected worktree remove --force as last worktree op, got %q (calls: %v)", last, sut.calls)
	}
}

// gitWorktreeVia wraps a Runner as a WorktreeRunner (used to route the git
// plumbing through the same recording runner).
func gitWorktreeVia(r Runner) WorktreeRunner {
	return gitWorktreeRunner{r: r}
}

type gitWorktreeRunner struct{ r Runner }

func (g gitWorktreeRunner) Add(repoPath, stagingPath, branch string) spawn.Result {
	return g.r.RunCli("git", []string{"worktree", "add", "--detach", stagingPath, branch}, spawn.Options{Dir: repoPath, TimeoutMs: 60_000})
}

func (g gitWorktreeRunner) Remove(repoPath, stagingPath string) spawn.Result {
	return g.r.RunCli("git", []string{"worktree", "remove", "--force", stagingPath}, spawn.Options{Dir: repoPath, TimeoutMs: 60_000})
}

func TestRunRegressionOracleRedSuiteBlocks(t *testing.T) {
	repo := gitRepo(t, nil)
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, "line-"+itoa(i))
	}
	sut := &recordingGitRunner{runner: &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		if name == "git" {
			cmd := exec.Command(name, args...)
			cmd.Dir = opts.Dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return spawn.Result{ExitCode: 1, Stderr: string(out)}
			}
			return spawn.Result{ExitCode: 0}
		}
		return spawn.Result{ExitCode: 1, Stdout: strings.Join(lines, "\n")}
	}}}
	en := true
	r, err := RunRegressionOracle(repo, "devagent/pull-1", RegressionOracleOptions{TimeoutMs: 10_000, Enabled: &en, Runner: sut, Worktrees: gitWorktreeVia(sut)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Passed || r.Skipped {
		t.Errorf("red suite must block, got %+v", r)
	}
	if r.Excerpt == "" {
		t.Fatal("expected an excerpt")
	}
	excerptLines := strings.Split(r.Excerpt, "\n")
	if len(excerptLines) != 15 {
		t.Errorf("excerpt lines = %d, want 15", len(excerptLines))
	}
	if excerptLines[len(excerptLines)-1] != "line-39" {
		t.Errorf("last excerpt line = %q, want line-39", excerptLines[len(excerptLines)-1])
	}
	if strings.Contains(r.Excerpt, "line-0\n") {
		t.Errorf("excerpt should not contain the head lines")
	}
}

func TestRunRegressionOracleSkipsWithoutTestCommand(t *testing.T) {
	repo := gitRepo(t, nil)
	dropPackageJson(t, repo)
	sut := &recordingGitRunner{runner: &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		if name == "git" {
			cmd := exec.Command(name, args...)
			cmd.Dir = opts.Dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return spawn.Result{ExitCode: 1, Stderr: string(out)}
			}
			return spawn.Result{ExitCode: 0}
		}
		return spawn.Result{ExitCode: 0}
	}}}
	en := true
	r, err := RunRegressionOracle(repo, "devagent/pull-1", RegressionOracleOptions{TimeoutMs: 10_000, Enabled: &en, Runner: sut, Worktrees: gitWorktreeVia(sut)})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Passed || !r.Skipped || r.Reason != ReasonNoTestCommand {
		t.Errorf("expected no-test-command skip, got %+v", r)
	}
}

func TestRunRegressionOracleDisabledKnob(t *testing.T) {
	repo := gitRepo(t, map[string]string{
		"devagent.json": `{"orchestrate": {"regressionOracle": false}}`,
	})
	sut := &fakeRunner{}
	r, err := RunRegressionOracle(repo, "devagent/pull-1", RegressionOracleOptions{TimeoutMs: 10_000, Runner: sut, Worktrees: gitWorktreeVia(sut)})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Passed || !r.Skipped || r.Reason != ReasonDisabled {
		t.Errorf("expected disabled skip, got %+v", r)
	}
	if len(sut.calls) != 0 {
		t.Errorf("disabled gate must not spawn anything: %v", sut.calls)
	}
}

func TestRunRegressionOracleKnobTrueFromRepoConfig(t *testing.T) {
	repo := gitRepo(t, map[string]string{
		"devagent.json":  `{"orchestrate": {"regressionOracle": true}}`,
		"pyproject.toml": "[tool.pytest]\n",
	})
	dropPackageJson(t, repo)
	var suiteArgs []string
	sut := &recordingGitRunner{runner: &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		if name == "git" {
			cmd := exec.Command(name, args...)
			cmd.Dir = opts.Dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return spawn.Result{ExitCode: 1, Stderr: string(out)}
			}
			return spawn.Result{ExitCode: 0}
		}
		if name == "python3" {
			suiteArgs = args
		}
		return spawn.Result{ExitCode: 0}
	}}}
	r, err := RunRegressionOracle(repo, "devagent/pull-1", RegressionOracleOptions{TimeoutMs: 10_000, Runner: sut, Worktrees: gitWorktreeVia(sut)})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Passed || r.Skipped {
		t.Errorf("expected run, got %+v", r)
	}
	// python convention resolved from the worktree
	if strings.Join(suiteArgs, " ") != "-m pytest" {
		t.Errorf("suite args = %v, want [-m pytest]", suiteArgs)
	}
}

func TestRunRegressionOracleNonJSRepoWithDeclaredTestCommand(t *testing.T) {
	repo := gitRepo(t, map[string]string{
		"devagent.json":  `{"testCommand": "make test"}`,
		"pyproject.toml": "[tool.pytest]\n",
	})
	dropPackageJson(t, repo)
	var suiteArgs []string
	sut := &recordingGitRunner{runner: &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		if name == "git" {
			cmd := exec.Command(name, args...)
			cmd.Dir = opts.Dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return spawn.Result{ExitCode: 1, Stderr: string(out)}
			}
			return spawn.Result{ExitCode: 0}
		}
		if name == "make" {
			suiteArgs = args
			return spawn.Result{ExitCode: 1, Stderr: "make: *** [test] Error 1"}
		}
		return spawn.Result{ExitCode: 0}
	}}}
	r, err := RunRegressionOracle(repo, "devagent/pull-1", RegressionOracleOptions{TimeoutMs: 10_000, Runner: sut, Worktrees: gitWorktreeVia(sut)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Passed || r.Skipped {
		t.Errorf("red make run must block, got %+v", r)
	}
	if strings.Join(suiteArgs, " ") != "test" {
		t.Errorf("suite args = %v, want [test]", suiteArgs)
	}
	if !strings.Contains(r.Excerpt, "make: *** [test] Error 1") {
		t.Errorf("excerpt = %q", r.Excerpt)
	}
}

func TestRunRegressionOracleNpmInstallWithLockfile(t *testing.T) {
	repo := gitRepo(t, map[string]string{
		"package-lock.json": `{"name": "fixture", "lockfileVersion": 3, "packages": {"": {}}}`,
	})
	var installArgs []string
	installCwd := ""
	sut := &recordingGitRunner{runner: &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		if name == "git" {
			cmd := exec.Command(name, args...)
			cmd.Dir = opts.Dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return spawn.Result{ExitCode: 1, Stderr: string(out)}
			}
			return spawn.Result{ExitCode: 0}
		}
		if name == "npm" && len(args) > 0 && args[0] == "ci" {
			installArgs = args
			installCwd = opts.Dir
		}
		return spawn.Result{ExitCode: 0}
	}}}
	en := true
	r, err := RunRegressionOracle(repo, "devagent/pull-1", RegressionOracleOptions{TimeoutMs: 10_000, Enabled: &en, Runner: sut, Worktrees: gitWorktreeVia(sut)})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Passed {
		t.Fatalf("expected pass, got %+v", r)
	}
	if strings.Join(installArgs, " ") != "ci --ignore-scripts" {
		t.Errorf("install args = %v", installArgs)
	}
	if !strings.Contains(installCwd, ".devagent-worktrees") {
		t.Errorf("install cwd = %q, want inside .devagent-worktrees", installCwd)
	}
}

func TestRunRegressionOracleInstallFailedSkipsFailOpen(t *testing.T) {
	repo := gitRepo(t, map[string]string{
		"package-lock.json": `{"name": "fixture", "lockfileVersion": 3, "packages": {"": {}}}`,
	})
	var suiteRan bool
	sut := &recordingGitRunner{runner: &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		if name == "git" {
			cmd := exec.Command(name, args...)
			cmd.Dir = opts.Dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return spawn.Result{ExitCode: 1, Stderr: string(out)}
			}
			return spawn.Result{ExitCode: 0}
		}
		if name == "npm" && len(args) > 0 && args[0] == "ci" {
			return spawn.Result{ExitCode: 1, Stderr: "npm ERR! network"}
		}
		suiteRan = true
		return spawn.Result{ExitCode: 0}
	}}}
	en := true
	r, err := RunRegressionOracle(repo, "devagent/pull-1", RegressionOracleOptions{TimeoutMs: 10_000, Enabled: &en, Runner: sut, Worktrees: gitWorktreeVia(sut)})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Passed || !r.Skipped || r.Reason != ReasonInstallFailed {
		t.Errorf("expected install-failed skip, got %+v", r)
	}
	// the red-on-missing-modules suite never ran
	if suiteRan {
		t.Error("suite must not run after a failed install")
	}
}

func TestRunRegressionOracleNoLockfileNoInstall(t *testing.T) {
	repo := gitRepo(t, nil)
	var sawNpmCi bool
	sut := &recordingGitRunner{runner: &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		if name == "git" {
			cmd := exec.Command(name, args...)
			cmd.Dir = opts.Dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return spawn.Result{ExitCode: 1, Stderr: string(out)}
			}
			return spawn.Result{ExitCode: 0}
		}
		if name == "npm" && len(args) > 0 && args[0] == "ci" {
			sawNpmCi = true
		}
		return spawn.Result{ExitCode: 0}
	}}}
	en := true
	if _, err := RunRegressionOracle(repo, "devagent/pull-1", RegressionOracleOptions{TimeoutMs: 10_000, Enabled: &en, Runner: sut, Worktrees: gitWorktreeVia(sut)}); err != nil {
		t.Fatal(err)
	}
	if sawNpmCi {
		t.Error("npm ci must not run without a lockfile")
	}
}

func TestRunRegressionOracleWorktreeAddFailed(t *testing.T) {
	repo := gitRepo(t, nil)
	var removeCalled bool
	sut := &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		if name == "git" && len(args) > 1 && args[0] == "worktree" && args[1] == "add" {
			return spawn.Result{ExitCode: 128, Stderr: "fatal: invalid reference"}
		}
		if name == "git" && len(args) > 1 && args[0] == "worktree" && args[1] == "remove" {
			removeCalled = true
		}
		return spawn.Result{ExitCode: 0}
	}}
	en := true
	r, err := RunRegressionOracle(repo, "devagent/pull-1", RegressionOracleOptions{TimeoutMs: 10_000, Enabled: &en, Runner: sut, Worktrees: gitWorktreeVia(sut)})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Passed || !r.Skipped || r.Reason != ReasonWorktreeFailed {
		t.Errorf("expected worktree-failed skip, got %+v", r)
	}
	if removeCalled {
		t.Error("remove must not run when add failed")
	}
}

func TestRunRegressionOracleWorktreeRemovedEvenWhenSuiteFails(t *testing.T) {
	repo := gitRepo(t, nil)
	var removed bool
	sut := &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		if name == "git" {
			cmd := exec.Command(name, args...)
			cmd.Dir = opts.Dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return spawn.Result{ExitCode: 1, Stderr: string(out)}
			}
			if len(args) > 1 && args[0] == "worktree" && args[1] == "remove" {
				removed = true
			}
			return spawn.Result{ExitCode: 0}
		}
		return spawn.Result{ExitCode: 1, Stdout: "suite red"}
	}}
	en := true
	r, err := RunRegressionOracle(repo, "devagent/pull-1", RegressionOracleOptions{TimeoutMs: 10_000, Enabled: &en, Runner: sut, Worktrees: gitWorktreeVia(sut)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Passed {
		t.Errorf("red suite must block: %+v", r)
	}
	if !removed {
		t.Error("worktree must be removed even when the suite fails (finally path)")
	}
}

func TestCheckRollupEvaluateChecks(t *testing.T) {
	// Pure evaluation of the CI rollup: any failure blocks, any run still
	// going means wait.
	allGreen := EvaluateChecks([]CheckRun{
		{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
	})
	if allGreen.Pending || !allGreen.Passed {
		t.Errorf("all green = %+v", allGreen)
	}
	if allGreen.Summary != "2/2 checks passed" {
		t.Errorf("summary = %q", allGreen.Summary)
	}

	pending := EvaluateChecks([]CheckRun{
		{Name: "build", Status: "IN_PROGRESS", Conclusion: ""},
		{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
	})
	if !pending.Pending || !pending.Passed {
		t.Errorf("pending = %+v", pending)
	}
	if pending.Summary != "1/2 checks passed, 1 running" {
		t.Errorf("summary = %q", pending.Summary)
	}

	failed := EvaluateChecks([]CheckRun{
		{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"},
		{Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"},
	})
	if failed.Pending || failed.Passed {
		t.Errorf("failed = %+v", failed)
	}
	if strings.Join(failed.FailedChecks, ";") != "test=FAILURE;lint=FAILURE" {
		t.Errorf("failedChecks = %v", failed.FailedChecks)
	}
	if !strings.Contains(failed.Summary, "failed: test, lint") {
		t.Errorf("summary = %q", failed.Summary)
	}

	// SKIPPED/NEUTRAL are not failures.
	skipped := EvaluateChecks([]CheckRun{{Name: "docs", Status: "COMPLETED", Conclusion: "SKIPPED"}})
	if !skipped.Passed {
		t.Errorf("skipped = %+v", skipped)
	}

	// TS: passed = failed.length === 0 — an empty rollup is vacuously green
	// (isAutoMergeReady adds the checks.length > 0 requirement separately).
	empty := EvaluateChecks(nil)
	if empty.Pending || !empty.Passed || empty.Summary != "no checks reported" {
		t.Errorf("empty = %+v", empty)
	}
}
