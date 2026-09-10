package loopdriver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/scout"
)

// agentCommand builds the devagent CLI invocation (DevagentArgs prepended).
func (d *driver) agentCommand(args ...string) *exec.Cmd {
	return d.agentCommandCtx(context.Background(), args...)
}

// agentCommandCtx is agentCommand built over ctx: the ctx wires
// CommandContext's Cancel, which the outer dispatch walls arm.
func (d *driver) agentCommandCtx(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, d.cfg.DevagentBin, append(append([]string{}, d.cfg.DevagentArgs...), args...)...)
	cmd.Dir = d.cfg.Repo
	return cmd
}

// devagentEnv rides DEVAGENT_VISIBILITY (plus extras) on a dispatch.
func (d *driver) devagentEnv(extra ...string) []string {
	env := append(os.Environ(), "DEVAGENT_VISIBILITY="+d.cfg.Visibility)
	return append(env, extra...)
}

// runDevagent runs a devagent CLI step whose output matters only as text
// (gates, sweeps); returns (combined output, rc).
func (d *driver) runDevagent(args ...string) (string, int) {
	cmd := d.agentCommand(args...)
	cmd.Env = d.devagentEnv()
	out, err := cmd.CombinedOutput()
	rc := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			rc = ee.ExitCode()
		} else {
			rc = 1
		}
	}
	return string(out), rc
}

// wallWaitDelay bounds Wait after a wall kill or a child exit whose output
// pipes are held by a grandchild (the #248 bounded-drain semantics,
// generalized to the outer dispatch): an orphaned worker session holding
// the pipes must not push the dispatch return past the wall.
const wallWaitDelay = 3 * time.Second

// runDevagentWithTimeout wraps runDevagent in the outer `timeout N` wall the
// bash driver applied to the task dispatch. The wall is a process-group
// kill: the dispatch child runs in its own group (setOwnProcessGroup) and,
// once the deadline passes, surviving group members are SIGKILLed
// (killDispatchTree). CommandContext kills only the direct child, and the
// child's descendants hold the output pipes — without the group sweep a
// wedged worker session outlives every anchor and pins the iteration
// (issue #273). WaitDelay keeps the Wait itself bounded in the same case.
func (d *driver) runDevagentWithTimeout(timeoutSecs int, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), secsToDuration(timeoutSecs))
	defer cancel()
	cmd := d.agentCommandCtx(ctx, args...)
	setOwnProcessGroup(cmd)
	cmd.WaitDelay = wallWaitDelay
	cmd.Env = d.devagentEnv(
		"DEVAGENT_API_MAX_ATTEMPTS="+itoa(d.cfg.APIMaxAttempts),
		"DEVAGENT_NO_PROGRESS_TIMEOUT_MS="+itoa(d.cfg.NoProgressTimeoutMS),
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// The rc resolves from ProcessState, not from Wait's error: Wait's
	// error conflates the wall (context deadline), the bounded-drain cutoff
	// (ErrWaitDelay) and the child's own exit — a child that finished 0
	// before the deadline must not be relabeled 124 by a drain that crossed
	// it (runTaskPhase maps a nonzero rc to a failed row + breaker bump).
	_ = runCmd(ctx, cmd)
	out := stdout.String() + stderr.String()
	// wallFired: the deadline passed. Sweep the group regardless of how the
	// child itself ended — nothing dispatched may outlive the anchor.
	wallFired := ctx.Err() == context.DeadlineExceeded
	if wallFired && cmd.Process != nil {
		killDispatchTree(cmd.Process.Pid)
	}
	return out, dispatchRc(cmd.ProcessState, wallFired)
}

// dispatchRc maps a finished dispatch to its exit code. It reads
// ProcessState, not Wait's error: that error conflates the wall (context
// deadline), the bounded-drain cutoff (ErrWaitDelay) and the child's own
// exit, and a child that finished 0 before the wall must not be relabeled
// 124 by a drain that crossed it — runTaskPhase turns a nonzero rc into a
// failed row + breaker bump. nil state = never started.
func dispatchRc(st *os.ProcessState, wallFired bool) int {
	if st == nil {
		return 1
	}
	if st.Success() {
		return 0
	}
	if wallFired {
		return 124 // GNU timeout convention
	}
	return st.ExitCode()
}

// paneRunDispatch performs the primary pane-run dispatch: `devagent
// pane-run --cwd REPO --timeout N --out RAW --err ERR --done DONE -- <bin
// words...> <prompt>` with stdout/stderr discarded. A non-zero rc is
// non-fatal; the caller falls back to a direct dispatch.
func (d *driver) paneRunDispatch(bin, prompt, outPath, errPath, donePath string, timeoutSecs int) int {
	args := []string{
		"pane-run",
		"--cwd", d.cfg.Repo,
		"--timeout", itoa(timeoutSecs),
		"--out", outPath,
		"--err", errPath,
		"--done", donePath,
		"--",
	}
	args = append(args, splitWords(bin)...)
	args = append(args, prompt)
	_, rc := d.runDevagent(args...)
	return rc
}

// directDispatch is the fallback when the pane-run done-marker never
// appeared: run the dispatch bin directly, stdout into rawPath (the bash
// `timeout N $BIN "$PROMPT" > raw || true` shape; rc is logged, not fatal).
// Returns the rc so the caller can log the exact message.
func (d *driver) directDispatch(bin, prompt, rawPath string, timeoutSecs int) int {
	words := splitWords(bin)
	ctx, cancel := context.WithTimeout(context.Background(), secsToDuration(timeoutSecs))
	defer cancel()
	cmd := exec.CommandContext(ctx, words[0], append(words[1:], prompt)...)
	cmd.Dir = d.cfg.Repo
	cmd.Stdin = nil // </dev/null
	raw, err := os.Create(rawPath)
	if err != nil {
		return 1
	}
	cmd.Stdout = raw
	runErr := runCmd(ctx, cmd)
	_ = raw.Close()
	if runErr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		return 124
	}
	return 1
}

// gradientScan ports the gradient scan-text step: `devagent scan-text` —
// the subcommand ignores arguments and prints the canonical GRADIENT
// adjacent-category text from src/research/scan-text.ts (consumed via
// scanPrompt here for documentation parity). Runs unbounded like the bash
// capture; a failed capture degrades to empty with the degrade note on
// stderr, never a dead driver.
func (d *driver) gradientScan() string {
	out, rc := d.runDevagent("scan-text", scanPrompt)
	if rc != 0 || strings.TrimSpace(out) == "" {
		_, _ = fmt.Fprintln(d.cfg.Stderr, "[gradient] scan-text dispatch failed — prompts run without the adjacent-category scan")
		return ""
	}
	return strings.TrimRight(out, "\n")
}

// failureClusters ports the `devagent ledger --clusters` capture: ranked
// recurring failures echoed into the research/PO prompts; empty + stderr
// degrade note on failure.
func (d *driver) failureClusters() string {
	out, rc := d.runDevagent("ledger", "--clusters", "--repo", d.cfg.Repo)
	if rc != 0 || strings.TrimSpace(out) == "" {
		_, _ = fmt.Fprint(d.cfg.Stderr, "[clusters] ledger --clusters capture failed — prompts run without the failure-cluster report\n")
		return ""
	}
	return strings.TrimRight(out, "\n")
}

// herdrSweep ports the herdr hygiene step: `devagent herdr-sweep --orphans`,
// last 3 output lines echoed into the iteration log; non-fatal when herdr
// is down.
func (d *driver) herdrSweep(logF io.Writer) {
	out, _ := d.runDevagent("herdr-sweep", "--orphans")
	for _, line := range tailLines(strings.TrimRight(out, "\n"), 3) {
		_, _ = fmt.Fprintln(logF, line)
	}
}

// preflight runs `devagent preflight --role <role> --repo REPO`; rc 0 =
// provider healthy.
func (d *driver) preflight(role string) bool {
	_, rc := d.runDevagent("preflight", "--role", role, "--repo", d.cfg.Repo)
	return rc == 0
}

// pageDegradeBreach fires the Q41 pager for a doc-sync degradation; best
// effort, never fatal, never delays the cycle.
func (d *driver) pageDegradeBreach(rc, detail string) {
	_, _ = d.runDevagent("page-degrade-breach", "--repo", d.cfg.Repo, "--source", "doc-sync",
		"--role", "selfbuild", "--detail", "doc-sync rc="+rc+": "+detail)
}

// syncDocsClass runs `devagent sync-docs --repo REPO` and returns (rc,
// combined output).
func (d *driver) syncDocsClass() (int, string) {
	out, rc := d.runDevagent("sync-docs", "--repo", d.cfg.Repo)
	return rc, out
}

// researchPaths bundles the per-iteration research artifact paths.
type researchPaths struct {
	raw   string
	out   string
	err   string
	done  string
	abort string
}

func (d *driver) researchPaths(loopNum int) researchPaths {
	dir := filepath.Join(d.stateDir, "research")
	return researchPaths{
		raw:   filepath.Join(dir, fmt.Sprintf("loop-%d.ndjson", loopNum)),
		out:   filepath.Join(dir, fmt.Sprintf("loop-%d.md", loopNum)),
		err:   filepath.Join(dir, fmt.Sprintf("loop-%d.err", loopNum)),
		done:  filepath.Join(dir, fmt.Sprintf(".loop-%d.done", loopNum)),
		abort: filepath.Join(dir, "last.aborted.ndjson"),
	}
}

// extractText ports the selfbuild-extract-text.mjs call: shape-based
// NDJSON → plain-text extraction into outPath via internal/scout.Extract,
// preserving the raw stream at abortPath when the stream carries no text
// (scout.Extract returns PreserveRawAt). A crashed helper must not kill
// the loop: outPath gets the extract-failed diagnostic (no "Goal:" prefix,
// so the gate rejects) instead of the raw blob.
func (d *driver) extractText(rawPath, outPath, abortPath, failDiag string) {
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		_ = os.WriteFile(outPath, []byte(failDiag), 0o644)
		return
	}
	res := scout.Extract(string(raw), abortPath)
	if res.PreserveRawAt != "" {
		_ = os.WriteFile(res.PreserveRawAt, raw, 0o644)
	}
	_ = os.WriteFile(outPath, []byte(res.Out), 0o644)
}

// runRepoTests runs the post-merge-back repo-level test gate: the word-split
// cfg.TestCmd (SELFBUILD_TEST_CMD, default `npm test`) inside cfg.Repo.
func (d *driver) runRepoTests() int {
	words := splitWords(d.cfg.TestCmd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, words[0], words[1:]...)
	cmd.Dir = d.cfg.Repo
	if err := runCmd(ctx, cmd); err != nil {
		return 1
	}
	return 0
}

// taskDispatch runs the phase-4 implementation dispatch
// (`devagent task --prompt GOAL --repo REPO --worker W [--model M]
// [--auto-pr]`) under the outer wall-clock cap; returns (combined output, rc).
// The output matters in pr push mode: a shipped iteration must carry a
// "PR opened: <url>" line before the driver may close the tracker issue
// (issue #238: soak-169 closed an issue with no PR behind it).
func (d *driver) taskDispatch(goal string) (string, int) {
	args := []string{"task", "--prompt", goal + "\n\n" + prdPolicy, "--repo", d.cfg.Repo, "--worker", d.cfg.Worker}
	if d.cfg.Model != "" {
		args = append(args, "--model", d.cfg.Model)
	}
	if d.cfg.PushMode == "pr" {
		args = append(args, "--auto-pr")
	}
	return d.runDevagentWithTimeout(d.cfg.TaskTimeout, args...)
}

// runCmd starts cmd and waits for it (ctx cancellation kills the process
// via CommandContext; this wrapper centralizes Start+Wait).
func runCmd(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Wait()
}

// splitWords mirrors the unquoted bash `-- $RESEARCH_BIN` word split.
func splitWords(s string) []string {
	return strings.Fields(s)
}
