package gates

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/spawn"
)

// Regression oracle (PRD §17 Phase 4, gate G6): before an auto-merge, check
// out the PR branch in a throwaway worktree and run the repo's full test
// suite there. A red suite blocks the merge; a skipped gate (no runnable
// test command / knob off / failed dependency install) lets auto-merge
// proceed. The worktree is always removed, even when the suite fails.

// RegressionOracleResult mirrors src/consume.ts RegressionOracleResult.
type RegressionOracleResult struct {
	// Passed is true when the suite is green, skipped, or disabled; false on
	// a red suite.
	Passed bool `json:"passed"`
	// Skipped is true when no runnable command was found or the gate is
	// disabled.
	Skipped bool `json:"skipped"`
	// Reason is why the gate skipped (or "" when it ran).
	Reason string `json:"reason,omitempty"`
	// Excerpt is the tail of the failing suite output.
	Excerpt string `json:"excerpt,omitempty"`
}

// Skip reasons (TS RegressionOracleResult['reason'] union).
const (
	ReasonNoTestCommand  = "no-test-command"
	ReasonDisabled       = "disabled"
	ReasonWorktreeFailed = "worktree-failed"
	ReasonInstallFailed  = "install-failed"
	ReasonMergeConflict  = "merge-conflict"
)

// WorktreeRunner is the minimal git-worktree lifecycle seam the regression
// oracle needs. TODO(FR-GO-03 #195): replace with the sibling port of
// src/git/worktree.ts (internal/git) once that wave-1 package lands; this
// local interface keeps this package independent of the in-flight sibling.
type WorktreeRunner interface {
	// Add materializes branch at stagingPath, mirroring
	// `git worktree add --detach <staging> <branch>` (60s timeout).
	Add(repoPath, stagingPath, branch string) spawn.Result
	// Remove force-removes the worktree, mirroring
	// `git worktree remove --force <staging>` (60s timeout).
	Remove(repoPath, stagingPath string) spawn.Result
}

// GitWorktreeRunner is the production seam backed by the git CLI.
type GitWorktreeRunner struct{}

// Add implements WorktreeRunner via `git worktree add --detach`.
func (GitWorktreeRunner) Add(repoPath, stagingPath, branch string) spawn.Result {
	return spawn.RunCli("git", []string{"worktree", "add", "--detach", stagingPath, branch},
		spawn.Options{Dir: repoPath, TimeoutMs: 60_000})
}

// Remove implements WorktreeRunner via `git worktree remove --force`.
func (GitWorktreeRunner) Remove(repoPath, stagingPath string) spawn.Result {
	return spawn.RunCli("git", []string{"worktree", "remove", "--force", stagingPath},
		spawn.Options{Dir: repoPath, TimeoutMs: 60_000})
}

// RegressionOracleOptions mirrors the TS opts object plus the seams.
type RegressionOracleOptions struct {
	TimeoutMs int
	// Enabled overrides the orchestrate.regressionOracle knob when non-nil.
	Enabled *bool
	// Runner executes the suite and npm ci. Nil → internal/spawn.
	Runner Runner
	// Worktrees overrides the git worktree seam. Nil → GitWorktreeRunner.
	Worktrees WorktreeRunner
	// Log mirrors the TS RunLogger: level is "info" or "warn". Nil → no-op.
	Log func(level, message string, extra map[string]any)
}

// RunRegressionOracle mirrors runRegressionOracle(). The detectTestCommand
// error surfaces like the TS throw; the worktree is still removed first.
func RunRegressionOracle(repoPath, branch string, opts RegressionOracleOptions) (RegressionOracleResult, error) {
	log := func(level, message string, extra map[string]any) {
		if opts.Log != nil {
			opts.Log(level, message, extra)
		}
	}

	enabled := opts.Enabled
	if enabled == nil {
		// TS loadOrchestrateConfig propagates config errors; here an invalid
		// config is treated as knob-unset (default on) — the caller-visible
		// gate behavior for valid configs is identical.
		if cfg, err := config.Load(repoPath); err == nil && cfg.Orchestrate != nil && cfg.Orchestrate.RegressionOracle != nil {
			enabled = cfg.Orchestrate.RegressionOracle
		}
	}
	if enabled != nil && !*enabled {
		return RegressionOracleResult{Passed: true, Skipped: true, Reason: ReasonDisabled}, nil
	}

	worktrees := opts.Worktrees
	if worktrees == nil {
		worktrees = GitWorktreeRunner{}
	}
	worktreesRoot := filepath.Join(repoPath, ".devagent-worktrees")
	if err := os.MkdirAll(worktreesRoot, 0o755); err != nil {
		return RegressionOracleResult{}, err
	}
	staging := filepath.Join(worktreesRoot, fmt.Sprintf("regression-%d-%s", time.Now().UnixMilli(), randToken6()))

	add := worktrees.Add(repoPath, staging, branch)
	if add.ExitCode != 0 {
		log("warn", "regression oracle worktree add failed; skipping gate", map[string]any{
			"gate":   "regression",
			"branch": branch,
			"stderr": truncateStr(add.Stderr, 200),
		})
		return RegressionOracleResult{Passed: true, Skipped: true, Reason: ReasonWorktreeFailed}, nil
	}

	result, err := runRegressionSuite(staging, branch, opts, log)

	// finally: the worktree is always removed, even when the suite fails.
	remove := worktrees.Remove(repoPath, staging)
	if remove.ExitCode != 0 {
		log("warn", "regression oracle worktree remove failed", map[string]any{
			"gate":   "regression",
			"path":   staging,
			"stderr": truncateStr(remove.Stderr, 200),
		})
	}
	return result, err
}

// runRegressionSuite mirrors the TS try-block body.
func runRegressionSuite(staging, branch string, opts RegressionOracleOptions, log func(string, string, map[string]any)) (RegressionOracleResult, error) {
	testCommand, err := DetectTestCommand(staging)
	if err != nil {
		return RegressionOracleResult{}, err
	}
	if testCommand == nil {
		return RegressionOracleResult{Passed: true, Skipped: true, Reason: ReasonNoTestCommand}, nil
	}

	// Fresh worktrees lack node_modules; npm suites with a lockfile get an
	// install first, and a failed install skips the gate (fail-open).
	if install := installSuiteDeps(staging, *testCommand, opts.TimeoutMs, opts.Runner, log); install != nil {
		return *install, nil
	}

	run := runWith(opts.Runner, testCommand.Cmd, testCommand.Args, spawn.Options{Dir: staging, TimeoutMs: opts.TimeoutMs})
	if run.ExitCode == 0 {
		log("info", "regression oracle passed", map[string]any{"gate": "regression", "branch": branch})
		return RegressionOracleResult{Passed: true, Skipped: false}, nil
	}
	log("warn", "regression oracle blocked merge: suite failed", map[string]any{
		"gate":     "regression",
		"branch":   branch,
		"exitCode": run.ExitCode,
	})
	combined := strings.TrimRightFunc(run.Stdout+run.Stderr, unicode.IsSpace)
	return RegressionOracleResult{Passed: false, Skipped: false, Excerpt: lastLines(combined, 15)}, nil
}

// installSuiteDeps mirrors installSuiteDeps(): run `npm ci --ignore-scripts`
// in a lockfile'd npm worktree so the suite sees node_modules (a fresh
// worktree ships without it). Returns a skip result when the install fails:
// the suite is not run, because a red run caused by missing modules would
// false-block every merge. Non-npm suites and repos without a lockfile need
// no install and return nil.
func installSuiteDeps(staging string, testCommand TestCommand, timeoutMs int, r Runner, log func(string, string, map[string]any)) *RegressionOracleResult {
	if testCommand.Cmd != "npm" {
		return nil
	}
	hasLockfile := false
	for _, f := range []string{"package-lock.json", "npm-shrinkwrap.json"} {
		if _, err := os.Stat(filepath.Join(staging, f)); err == nil {
			hasLockfile = true
			break
		}
	}
	if !hasLockfile {
		return nil
	}
	install := runWith(r, "npm", []string{"ci", "--ignore-scripts"}, spawn.Options{Dir: staging, TimeoutMs: timeoutMs})
	if install.ExitCode == 0 {
		return nil
	}
	log("warn", "regression oracle skipped: dependency install failed", map[string]any{
		"gate":   "regression",
		"stderr": truncateStr(install.Stderr, 200),
	})
	return &RegressionOracleResult{Passed: true, Skipped: true, Reason: ReasonInstallFailed}
}

// randToken6 mirrors the TS Math.random().toString(36).slice(2, 8) suffix.
func randToken6() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "000000"
	}
	return fmt.Sprintf("%x", b)[:6]
}

// lastLines mirrors the TS `.split('\n').slice(-n).join('\n')` excerpt tail.
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ---------- CI check rollup (src/integrations/autopr.ts evaluateChecks) ----------

// CheckRun mirrors src/integrations/autopr.ts CheckRun. Conclusion is ""
// where the TS value is null (no conclusion yet).
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// ChecksVerdict mirrors src/integrations/autopr.ts ChecksVerdict.
type ChecksVerdict struct {
	Pending      bool     `json:"pending"`
	Passed       bool     `json:"passed"`
	FailedChecks []string `json:"failedChecks"`
	Summary      string   `json:"summary"`
}

// checkPassed mirrors the TS checkPassed: true when a completed check counts
// as passing (SKIPPED/NEUTRAL are not failures; "" is the TS null).
func checkPassed(c CheckRun) bool {
	switch c.Conclusion {
	case "SUCCESS", "SKIPPED", "NEUTRAL":
		return true
	}
	return false
}

// EvaluateChecks mirrors the TS evaluateChecks: pure evaluation of the CI
// rollup — any failure blocks, any run still going means wait.
func EvaluateChecks(checks []CheckRun) ChecksVerdict {
	running := 0
	passed := 0
	var failed []CheckRun
	for _, c := range checks {
		if c.Status != "COMPLETED" {
			running++
			continue
		}
		if checkPassed(c) {
			passed++
		} else {
			failed = append(failed, c)
		}
	}
	failedChecks := []string{}
	failedNames := []string{}
	for _, c := range failed {
		failedChecks = append(failedChecks, fmt.Sprintf("%s=%s", c.Name, c.Conclusion))
		failedNames = append(failedNames, c.Name)
	}
	summary := "no checks reported"
	if len(checks) > 0 {
		summary = fmt.Sprintf("%d/%d checks passed", passed, len(checks))
		if running > 0 {
			summary += fmt.Sprintf(", %d running", running)
		}
		if len(failed) > 0 {
			summary += ", failed: " + strings.Join(failedNames, ", ")
		}
	}
	return ChecksVerdict{
		Pending:      running > 0,
		Passed:       len(failed) == 0,
		FailedChecks: failedChecks,
		Summary:      summary,
	}
}
