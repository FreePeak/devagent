// Package file mirrors src/integrations/autopr.ts (CI-fixer state machine,
// batch entry point, and zombie-PR sweep — FR-GO-07, issue #194). Split from
// autopr.go so the pure decision gates stay in one file and the live-loop
// plumbing sits here; same package.
package orchestrator

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/gates"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/queue"
	"github.com/FreePeak/devagent/internal/spawn"
)

// AutoMergeOutcome mirrors the TS AutoMergeOutcome.
type AutoMergeOutcome struct {
	PR     int    `json:"pr"`
	Title  string `json:"title"`
	Action string `json:"action"`
	Detail string `json:"detail"`
	// CI-Fixer failure evidence (Q24 error taxonomy); set on
	// 'ci-fix-failed' outcomes.
	FailedChecks []string `json:"failedChecks,omitempty"`
	Attempts     *int     `json:"attempts,omitempty"`
	Summary      *string  `json:"summary,omitempty"`
}

// AutoMerge action literals (TS union 'merged' | 'review-requested' |
// 'skipped' | 'ci-fix-failed').
const (
	ActionMerged          = "merged"
	ActionReviewRequested = "review-requested"
	ActionSkipped         = "skipped"
	ActionCiFixFailed     = "ci-fix-failed"
)

// CiFixRequest mirrors the TS CiFixRequest.
type CiFixRequest struct {
	RepoPath string
	PR       int
	// Task identity for the re-dispatched run (TASK-fix-<pr>).
	TaskID       string
	FailedChecks []string
	Prompt       string
}

// CiFixResult mirrors the TS { ok, note } fixer return.
type CiFixResult struct {
	OK   bool
	Note string
}

// CiFixer mirrors the TS CiFixer type.
type CiFixer func(req CiFixRequest) CiFixResult

// CiFixPrompt builds the fixer prompt (extracted verbatim from the TS
// inline construction so tests can pin it byte-for-byte).
func CiFixPrompt(repoPath string, pr int, failedChecks []string) string {
	return strings.Join([]string{
		fmt.Sprintf("Fix the failing CI checks on PR #%d (%s).", pr, repoPath),
		fmt.Sprintf("Failed checks: %s.", strings.Join(failedChecks, ", ")),
		"Reproduce locally, apply the minimal fix, push to the PR branch, and let CI re-run.",
	}, " ")
}

// DecideCiFixRetry is the extracted CI-fixer re-poll decision function: the
// outcome the loop must record after a successful dispatch, given the fresh
// verdict. Pure — fixture-tested without gh.
//   - ""       keep polling (checks still pending)
//   - failed-then-green: merge path
//   - still-red: structured ci-fix-failed surrender
func DecideCiFixRetry(cv gates.ChecksVerdict) string {
	if cv.Pending {
		return ""
	}
	if cv.Passed {
		return "failed-then-green"
	}
	return "still-red"
}

// ciFixOutcomeStillRedRow picks the failedChecks for the still-red ledger
// row: the fresh rollup's failures when present, else the originally
// dispatched ones (TS `fixCv.failedChecks.length ? ... : failedChecks`).
func ciFixOutcomeStillRedRow(fresh, dispatched []string) []string {
	if len(fresh) > 0 {
		return fresh
	}
	return dispatched
}

// AutoReviewAndMergeOptions mirrors the TS AutoReviewAndMergeOptions.
type AutoReviewAndMergeOptions struct {
	// Only PRs targeting this branch are considered ("" = no filter).
	BaseBranch string
	// squash | merge | rebase ("" = squash).
	Method string
	// DeleteBranch nil = true (TS default).
	DeleteBranch *bool
	DryRun       bool
	// Seconds to wait for pending checks before giving up (nil = 300).
	WaitForChecksSec *int
	// Milliseconds between check polls (nil = 15_000).
	PollIntervalMs *int
	// CI-Fixer: when failed checks block the merge, one bounded re-dispatch
	// (nil = DefaultCiFixer) runs before the verdict falls back to
	// request-changes; the merge only proceeds if checks are green afterward.
	Fixer CiFixer
	// Merge-queue grace window in hours: a PR red across it is skipped with
	// a red-across-grace reason instead of parking the pipeline (nil =
	// config prHygiene.graceHours, itself defaulting to 24).
	GraceHours *float64
	// PR number superseding this one on the same base (computed by the
	// batch entry point): a non-candidate head is skipped with a superseded
	// reason.
	SupersedingCandidate *int
}

// AutoReviewAndMergeOneOpts carries the per-call log hook alongside the
// options object (TS `opts & { log? }`).
type AutoReviewAndMergeOneOpts struct {
	AutoReviewAndMergeOptions
	Log func(msg string)
}

// AutoReviewAndMergeOne mirrors the TS autoReviewAndMergeOne: process one
// PR end-to-end — status -> diff hazard scan -> review -> merge.
func AutoReviewAndMergeOne(repoPath string, pr int, opts AutoReviewAndMergeOneOpts, run RunGh) AutoMergeOutcome {
	logFn := opts.Log
	if logFn == nil {
		logFn = func(string) {}
	}
	method := opts.Method
	if method == "" {
		method = "squash"
	}
	deleteBranch := true
	if opts.DeleteBranch != nil {
		deleteBranch = *opts.DeleteBranch
	}

	status, err := GetPrStatus(repoPath, pr, run)
	if err != nil {
		return AutoMergeOutcome{PR: pr, Action: ActionSkipped, Detail: "cannot read PR: " + sliceUTF16(err.Error(), 200)}
	}

	if opts.BaseBranch != "" && status.BaseRefName != opts.BaseBranch {
		return AutoMergeOutcome{PR: pr, Title: status.Title, Action: ActionSkipped, Detail: fmt.Sprintf("base is %s, not %s", status.BaseRefName, opts.BaseBranch)}
	}
	if status.State != "OPEN" {
		return AutoMergeOutcome{PR: pr, Title: status.Title, Action: ActionSkipped, Detail: fmt.Sprintf("state is %s", status.State)}
	}

	// Wait for pending checks so we never judge an incomplete rollup
	waitSecs := 300
	if opts.WaitForChecksSec != nil {
		waitSecs = *opts.WaitForChecksSec
	}
	deadline := timeNowMs() + int64(waitSecs)*1000
	var cv gates.ChecksVerdict
	for {
		cv = EvaluateChecksOf(status)
		if !cv.Pending {
			break
		}
		if timeNowMs() >= deadline {
			return AutoMergeOutcome{PR: pr, Title: status.Title, Action: ActionSkipped, Detail: fmt.Sprintf("checks still pending after timeout: %s", cv.Summary)}
		}
		logFn(fmt.Sprintf("checks pending (%s); retrying in 15s", cv.Summary))
		sleepMs(pollIntervalMs(opts))
		status, err = GetPrStatus(repoPath, pr, run)
		if err != nil {
			return AutoMergeOutcome{PR: pr, Title: status.Title, Action: ActionSkipped, Detail: "cannot read PR: " + sliceUTF16(err.Error(), 200)}
		}
	}

	// Merge-queue gate 1 (base-superseded): a PR whose head base branch was
	// merged or deleted can never integrate — waiting cannot fix a dead
	// base, so skip it with a reason instead of parking the pipeline.
	// Auto-close of such PRs stays owned by the pr-hygiene sweep. A probe
	// hiccup cannot be distinguished from a dead base through the seam's
	// boolean — the TS try/catch swallows probe errors into "fall through";
	// the scripted seam mirrors gh by throwing only on real 404s, so this
	// port probes through an error-carrying call and treats transport noise
	// as alive (matching the tolerant TS behavior for non-404 failures).
	if BaseBranchGone(repoPath, status.BaseRefName, run) {
		return AutoMergeOutcome{
			PR:     pr,
			Title:  status.Title,
			Action: ActionSkipped,
			Detail: fmt.Sprintf("base-superseded: base %s was merged or deleted; branch can never integrate", status.BaseRefName),
		}
	}

	// Merge-queue gate 2 (red-across-grace / superseded): skip PRs that
	// would only park the queue — the CI-Fixer below runs for
	// red-within-grace PRs.
	graceHours := opts.GraceHours
	if graceHours == nil {
		defaultGrace := 24.0
		graceHours = &defaultGrace
		if cfg, err := config.Load(repoPath); err == nil && cfg.PRHygiene != nil && cfg.PRHygiene.GraceHours != nil {
			graceHours = cfg.PRHygiene.GraceHours
		}
	}
	gate := EvaluateMergeQueueGate(status, MergeQueueGateOptions{
		GraceHours:           graceHours,
		SupersedingCandidate: opts.SupersedingCandidate,
	})
	if gate.Skip {
		return AutoMergeOutcome{PR: pr, Title: status.Title, Action: ActionSkipped, Detail: fmt.Sprintf("%s: %s", gate.Reason, gate.Detail)}
	}

	var hazards []gates.Finding
	if diff, err := GetPrDiff(repoPath, pr, run); err == nil {
		hazards = ScanAddedLinesForHazards(diff)
		// diff unavailable (empty PR or gh hiccup): proceed with CI +
		// mergeability only
	}

	review := EvaluateAutoReview(status, ReviewEvidence{Hazards: hazards, MergeMethod: method})

	if opts.DryRun {
		return AutoMergeOutcome{PR: pr, Title: status.Title, Action: ActionSkipped, Detail: "[dry-run] " + review.Reason}
	}

	// CI-Fixer (PRD.md:737): when failed checks are the blocker, give the PR
	// one bounded repair re-dispatch before falling back to request-changes.
	// The verdict is re-derived from a fresh status poll so only genuinely
	// green checks can reach the merge below (never worker-graded results).
	if review.Event == ReviewEventRequestChanges && len(cv.FailedChecks) > 0 {
		return ciFixLoop(repoPath, pr, status, cv.FailedChecks, hazards, method, deleteBranch, opts, run, logFn)
	}

	return FinishAutoMerge(repoPath, pr, status, review, method, deleteBranch, run, logFn)
}

// pollIntervalMs mirrors the TS opts.pollIntervalMs ?? 15_000.
func pollIntervalMs(opts AutoReviewAndMergeOneOpts) int {
	if opts.PollIntervalMs != nil {
		return *opts.PollIntervalMs
	}
	return 15_000
}

// waitChecksSec mirrors the TS opts.waitForChecksSec ?? 300.
func waitChecksSec(opts AutoReviewAndMergeOneOpts) int {
	if opts.WaitForChecksSec != nil {
		return *opts.WaitForChecksSec
	}
	return 300
}

// ciFixLoop mirrors the TS inline CI-fixer block: dispatch once, record
// ledger rows (ci-fix-dispatched / ci-fix-outcome), re-poll to a completed
// rollup, then either merge (failed-then-green) or surrender with a
// structured ci-fix-failed outcome (still-red / dispatch failure / poll
// timeout). Every decision routes through the pure DecideCiFixRetry /
// ciFixOutcomeStillRedRow helpers so the sequences stay fixture-testable.
func ciFixLoop(repoPath string, pr int, status PrStatus, failedChecks []string, hazards []gates.Finding, method string, deleteBranch bool, opts AutoReviewAndMergeOneOpts, run RunGh, logFn func(string)) AutoMergeOutcome {
	fixer := opts.Fixer
	if fixer == nil {
		fixer = DefaultCiFixer
	}
	prompt := CiFixPrompt(repoPath, pr, failedChecks)
	taskID := fmt.Sprintf("TASK-fix-%d", pr)
	logFn(fmt.Sprintf("ci-fix: re-dispatching fixer for PR #%d (failed: %s)", pr, strings.Join(failedChecks, ", ")))
	req := CiFixRequest{RepoPath: repoPath, PR: pr, TaskID: taskID, FailedChecks: failedChecks, Prompt: prompt}
	fixRes, dispatchErr := catchCiFix(func() CiFixResult { return fixer(req) })
	if dispatchErr != nil {
		appendCiFixOutcome(repoPath, taskID, pr, failedChecks, "ci-fix-failed", sliceUTF16(dispatchErr.Error(), 200))
		attempts := 1
		summary := EvaluateChecksOf(status).Summary
		return AutoMergeOutcome{
			PR:           pr,
			Title:        status.Title,
			Action:       ActionCiFixFailed,
			Detail:       "ci-fix dispatch threw: " + sliceUTF16(dispatchErr.Error(), 200),
			FailedChecks: failedChecks,
			Attempts:     &attempts,
			Summary:      &summary,
		}
	}
	if !fixRes.OK {
		// Nothing was dispatched: record the structured failure and skip the
		// re-poll entirely (no pointless CI wait on an unchanged head SHA).
		appendCiFixOutcome(repoPath, taskID, pr, failedChecks, "ci-fix-failed", sliceUTF16(fixRes.Note, 200))
		attempts := 1
		summary := EvaluateChecksOf(status).Summary
		return AutoMergeOutcome{
			PR:           pr,
			Title:        status.Title,
			Action:       ActionCiFixFailed,
			Detail:       "ci-fix dispatch failed: " + sliceUTF16(fixRes.Note, 200),
			FailedChecks: failedChecks,
			Attempts:     &attempts,
			Summary:      &summary,
		}
	}
	// Dispatch succeeded: record the fixer round-trip start (goal id, PR
	// number, failed check names) so analytics can count fix attempts per
	// goal.
	appendCiFixDispatched(repoPath, taskID, pr, failedChecks)

	// Re-poll to a completed rollup on the new head SHA, then re-evaluate.
	fixDeadline := timeNowMs() + int64(waitChecksSec(opts))*1000
	for {
		fresh, err := GetPrStatus(repoPath, pr, run)
		if err != nil {
			appendCiFixOutcome(repoPath, taskID, pr, failedChecks, "ci-fix-failed", sliceUTF16(err.Error(), 200))
			attempts := 1
			summary := EvaluateChecksOf(status).Summary
			return AutoMergeOutcome{
				PR:           pr,
				Title:        status.Title,
				Action:       ActionCiFixFailed,
				Detail:       "cannot read PR: " + sliceUTF16(err.Error(), 200),
				FailedChecks: failedChecks,
				Attempts:     &attempts,
				Summary:      &summary,
			}
		}
		status = fresh
		fixCv := EvaluateChecksOf(status)
		switch decision := DecideCiFixRetry(fixCv); decision {
		case "":
			// keep polling below
		case "failed-then-green":
			logFn(fmt.Sprintf("ci-fix: checks green after fix dispatch; merging PR #%d", pr))
			appendCiFixOutcome(repoPath, taskID, pr, failedChecks, "failed-then-green", fixCv.Summary)
			fixedReview := EvaluateAutoReview(status, ReviewEvidence{Hazards: hazards, MergeMethod: method})
			return FinishAutoMerge(repoPath, pr, status, fixedReview, method, deleteBranch, run, logFn)
		case "still-red":
			rowChecks := ciFixOutcomeStillRedRow(fixCv.FailedChecks, failedChecks)
			appendCiFixOutcome(repoPath, taskID, pr, rowChecks, "still-red", fixCv.Summary)
			attempts := 1
			summary := fixCv.Summary
			return AutoMergeOutcome{
				PR:           pr,
				Title:        status.Title,
				Action:       ActionCiFixFailed,
				Detail:       fmt.Sprintf("checks still failing after 1 fix attempt: %s", fixCv.Summary),
				FailedChecks: rowChecks,
				Attempts:     &attempts,
				Summary:      &summary,
			}
		}
		if timeNowMs() >= fixDeadline {
			appendCiFixOutcome(repoPath, taskID, pr, failedChecks, "ci-fix-failed", fixCv.Summary)
			attempts := 1
			summary := fixCv.Summary
			return AutoMergeOutcome{
				PR:           pr,
				Title:        status.Title,
				Action:       ActionCiFixFailed,
				Detail:       fmt.Sprintf("checks still pending after fix attempt: %s", fixCv.Summary),
				FailedChecks: failedChecks,
				Attempts:     &attempts,
				Summary:      &summary,
			}
		}
		logFn(fmt.Sprintf("ci-fix: checks pending (%s); retrying in 15s", fixCv.Summary))
		sleepMs(pollIntervalMs(opts))
	}
}

// catchCiFix converts a panicking fixer into the TS thrown-error channel.
func catchCiFix(fn func() CiFixResult) (res CiFixResult, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("%v", rec)
		}
	}()
	return fn(), nil
}

// appendCiFixOutcome mirrors the TS appendFixerRecord ci-fix-outcome rows.
func appendCiFixOutcome(repoPath, taskID string, pr int, failedChecks []string, outcome, detail string) {
	ledger.AppendFixerRecord(repoPath, ledger.FixerRecord{
		TS:           queue.NowIso(),
		Kind:         "event",
		TaskID:       taskID,
		Attempt:      1,
		Event:        "ci-fix-outcome",
		PR:           pr,
		FailedChecks: failedChecks,
		Outcome:      &outcome,
		Detail:       &detail,
	})
}

// appendCiFixDispatched mirrors the TS appendFixerRecord ci-fix-dispatched
// rows (no outcome/detail keys — nil pointers omit them).
func appendCiFixDispatched(repoPath, taskID string, pr int, failedChecks []string) {
	ledger.AppendFixerRecord(repoPath, ledger.FixerRecord{
		TS:           queue.NowIso(),
		Kind:         "event",
		TaskID:       taskID,
		Attempt:      1,
		Event:        "ci-fix-dispatched",
		PR:           pr,
		FailedChecks: failedChecks,
	})
}

// FinishAutoMerge mirrors the TS finishAutoMerge: post the verdict, then
// attempt the merge; shared by the direct and CI-fixed paths.
func FinishAutoMerge(repoPath string, pr int, status PrStatus, review AutoReview, method string, deleteBranch bool, run RunGh, logFn func(string)) AutoMergeOutcome {
	selfAuthored := false
	if login, err := GetViewerLogin(repoPath, run); err == nil {
		selfAuthored = status.Author != "" && status.Author == login
		// viewer lookup failed: assume not self-authored and let the review
		// fail loudly
	}
	if selfAuthored {
		_ = PostPrComment(repoPath, pr, review.Body, run)
		logFn(fmt.Sprintf("verdict commented (self-authored): %s", review.Reason))
		if review.Event == ReviewEventRequestChanges {
			return AutoMergeOutcome{PR: pr, Title: status.Title, Action: ActionReviewRequested, Detail: review.Reason}
		}
	} else {
		_ = PostPrReview(repoPath, pr, review.Event, review.Body, run)
		logFn(fmt.Sprintf("review posted: %s", review.Reason))
		if review.Event == ReviewEventRequestChanges {
			return AutoMergeOutcome{PR: pr, Title: status.Title, Action: ActionReviewRequested, Detail: review.Reason}
		}
	}

	if err := MergePr(repoPath, pr, method, deleteBranch, run); err != nil {
		// mergePr already retried with --auto; a remaining failure means
		// GitHub still refuses (protection rules, permissions), so leave the
		// approval posted.
		return AutoMergeOutcome{PR: pr, Title: status.Title, Action: ActionReviewRequested, Detail: "approve succeeded but merge failed: " + sliceUTF16(err.Error(), 200)}
	}
	return AutoMergeOutcome{PR: pr, Title: status.Title, Action: ActionMerged, Detail: review.Reason}
}

// DefaultCiFixer mirrors the TS defaultCiFixer: built-in CI-Fixer dispatch —
// delegate a repair run to the shared host over SSH when DEVAGENT_REMOTE_TARGET
// is set; otherwise fall back to LocalCiFixer (repair in-place on this host)
// so a red PR always gets a fix attempt instead of a structured surrender.
func DefaultCiFixer(req CiFixRequest) CiFixResult {
	target := os.Getenv("DEVAGENT_REMOTE_TARGET")
	if target == "" {
		local := LocalCiFixer(req)
		if !local.OK {
			return CiFixResult{OK: false, Note: "ci-fix local dispatch: " + local.Note}
		}
		return local
	}
	res := remoteTaskSeam(RemoteTaskRequest{
		Target:    target,
		Prompt:    req.Prompt,
		TaskID:    req.TaskID,
		TimeoutMs: remoteFixTimeoutMs(req.RepoPath),
	}, RemoteTaskDeps{
		Run: func(argv []string, timeoutMs int) spawn.Result {
			return spawn.RunCli(argv[0], argv[1:], spawn.Options{Dir: req.RepoPath, TimeoutMs: timeoutMs})
		},
	})
	return CiFixResult{OK: res.OK, Note: res.Note}
}

// RemoteTaskRequest mirrors the TS runRemoteTask opts subset DefaultCiFixer
// passes.
type RemoteTaskRequest struct {
	Target    string
	Prompt    string
	TaskID    string
	TimeoutMs int
}

// RemoteTaskResult mirrors the TS RemoteRunResult subset consumed here.
type RemoteTaskResult struct {
	OK    bool
	PrURL string
	Note  string
}

// RemoteTaskDeps mirrors the TS RemoteDeps { run }.
type RemoteTaskDeps struct {
	Run func(argv []string, timeoutMs int) spawn.Result
}

// remoteTaskSeam is the injectable seam var DefaultCiFixer dispatches
// through (tests swap it; production resolves to defaultRemoteTaskSeam).
var remoteTaskSeam = defaultRemoteTaskSeam

// defaultRemoteTaskSeam is the production transport.
func defaultRemoteTaskSeam(opts RemoteTaskRequest, deps RemoteTaskDeps) RemoteTaskResult {
	return RunRemoteTaskSeam(opts, deps)
}

// RunRemoteTaskSeam is the SSH remote-task transport.
//
// TODO(FR-GO-07): replace with the internal/remote port when it lands; this
// seam reproduces runRemoteTask's preflight + dispatch contract (probe the
// host, `devagent task <prompt> --auto-pr --id <taskId>`) so DefaultCiFixer
// behavior is preserved.
func RunRemoteTaskSeam(opts RemoteTaskRequest, deps RemoteTaskDeps) RemoteTaskResult {
	host, path, ok := parseRemoteTarget(opts.Target)
	if !ok {
		return RemoteTaskResult{OK: false, Note: "invalid remote target: " + opts.Target}
	}
	// Preflight: fail fast (cheap probe) instead of burning the full timeout
	// on an unreachable host or missing devagent install (loop-59 hang
	// lesson).
	preflightCmd := fmt.Sprintf("command -v devagent >/dev/null && test -d %s && git -C %s rev-parse --git-dir >/dev/null", shellQuote(path), shellQuote(path))
	preflight := deps.Run(buildSshArgs(host, preflightCmd), minInt(opts.TimeoutMs, 15_000))
	if preflight.ExitCode != 0 {
		return RemoteTaskResult{
			OK:   false,
			Note: fmt.Sprintf("remote preflight failed on %s: need devagent on PATH and a git repo at %s", host, path),
		}
	}
	parts := []string{
		fmt.Sprintf("cd %s", shellQuote(path)),
		fmt.Sprintf("devagent task %s --auto-pr", shellQuote(opts.Prompt)),
	}
	if opts.TaskID != "" {
		parts = append(parts, fmt.Sprintf("--id %s", shellQuote(opts.TaskID)))
	}
	dispatch := deps.Run(buildSshArgs(host, strings.Join(parts, " && ")), opts.TimeoutMs)
	prURL := extractPrURL(dispatch.Stdout)
	if dispatch.ExitCode != 0 {
		note := fmt.Sprintf("remote task failed on %s (exit %d)", host, dispatch.ExitCode)
		if prURL != "" {
			note += fmt.Sprintf(", PR opened anyway: %s", prURL)
		}
		return RemoteTaskResult{OK: false, PrURL: prURL, Note: note}
	}
	if prURL != "" {
		return RemoteTaskResult{OK: true, PrURL: prURL, Note: "remote PR opened: " + prURL}
	}
	return RemoteTaskResult{OK: true, Note: "remote task finished without a PR URL"}
}

func parseRemoteTarget(target string) (host, path string, ok bool) {
	// TS parseRemoteTarget accepts [user@]host:/abs/path
	idx := strings.Index(target, ":")
	if idx <= 0 || !strings.HasPrefix(target[idx+1:], "/") {
		return "", "", false
	}
	return target[:idx], target[idx+1:], true
}

func buildSshArgs(host, command string) []string {
	return []string{"ssh", host, command}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func extractPrURL(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://github.com/") && strings.Contains(line, "/pull/") {
			return line
		}
	}
	return ""
}

func remoteFixTimeoutMs(repoPath string) int {
	if cfg, err := config.Load(repoPath); err == nil {
		return cfg.TimeoutMinutes * 60_000
	}
	return 30 * 60_000
}

// localCiFixerSeam is the injectable seam LocalCiFixer delegates to (tests
// swap runGh and the dispatcher; nil dispatcher keeps config resolution).
var localCiFixerSeam = func(req CiFixRequest, runGh RunGh, dispatcher WorkerDispatcher) CiFixResult {
	return localCiFixerCore(req, runGh, dispatcher)
}

// LocalCiFixer mirrors the TS localCiFixer: local CI-Fixer dispatch — repair
// the PR in-place on this host when no remote pool is configured. Flow:
// checkout the PR head branch into a disposable worktree, run the configured
// worker with the fix prompt, commit everything, push with
// --force-with-lease, remove the worktree. The re-poll in
// autoReviewAndMergeOne judges the result from a fresh CI rollup — never
// from the fixer's own claim — so a no-op fix cannot turn into a merge.
//
// The worker leg runs through WorkerDispatcher (the workers adapter is a
// sibling port); gh head-branch resolution runs through the RunGh seam.
func LocalCiFixer(req CiFixRequest) CiFixResult {
	return localCiFixerSeam(req, DefaultRunGh, nil)
}

// localCiFixerCore is the seam-injectable implementation.
func localCiFixerCore(req CiFixRequest, runGh RunGh, dispatcher WorkerDispatcher) CiFixResult {
	runGit := func(args []string, cwd string, timeoutMs int) (string, error) {
		r := spawn.RunCli("git", args, spawn.Options{Dir: cwd, TimeoutMs: timeoutMs})
		if r.ExitCode != 0 {
			return "", fmt.Errorf("git %s exited %d: %s", args[0], r.ExitCode, sliceUTF16(r.Stderr, 200))
		}
		return strings.TrimSpace(r.Stdout), nil
	}

	// Resolve the PR's head branch (owner:branch form from gh pr view).
	head, err := runGh([]string{"pr", "view", strconv.Itoa(req.PR), "--json", "headRefName", "--jq", ".headRefName"}, req.RepoPath)
	if err != nil {
		return CiFixResult{OK: false, Note: "cannot resolve PR head branch: " + sliceUTF16(err.Error(), 120)}
	}
	branch := strings.TrimSpace(head.Stdout)
	if branch == "" {
		return CiFixResult{OK: false, Note: "PR head branch is empty"}
	}

	// Fetch the head branch tip, then check it out in a disposable worktree.
	wt := req.RepoPath + "/.devagent-worktrees/TASK-fix-" + strconv.Itoa(req.PR)
	defer func() {
		_ = spawn.RunCli("git", []string{"worktree", "remove", "--force", wt}, spawn.Options{Dir: req.RepoPath, TimeoutMs: 30_000})
	}()
	fail := func(err error) CiFixResult {
		return CiFixResult{OK: false, Note: "local fixer failed: " + sliceUTF16(err.Error(), 200)}
	}
	if _, err := runGit([]string{"fetch", "origin", branch}, req.RepoPath, 60_000); err != nil {
		return fail(err)
	}
	// Worktree on a detached HEAD of the fetched tip: pushing back to the
	// same branch never conflicts with a locally-checked-out branch, and the
	// lease (FETCH_HEAD) still guards against a concurrent push.
	if _, err := runGit([]string{"worktree", "add", "--detach", wt, "FETCH_HEAD"}, req.RepoPath, 60_000); err != nil {
		return fail(err)
	}

	// Run the configured worker with the fix prompt inside the worktree.
	cfg, err := config.Load(req.RepoPath)
	if err != nil {
		return fail(err)
	}
	timeoutMs := cfg.TimeoutMinutes * 60_000
	if cfg.Worker == "both" {
		return CiFixResult{OK: false, Note: "local fixer requires a single worker, not fan-out (worker=both)"}
	}
	workerName := cfg.Worker
	if dispatcher == nil {
		// TODO(FR-GO-05 #190): replace with sibling port; until then the
		// local fixer cannot run a real worker — report the structured
		// failure like the TS spawn failure path.
		return CiFixResult{OK: false, Note: fmt.Sprintf("local fixer failed: no worker dispatcher registered for %s (workers port pending)", workerName)}
	}
	// Reviewer fixer model pin (operator spec: all agents on combo dev):
	// provider-qualified config.model ("provider/model") is forwarded to the
	// worker so reviews/CI-fixes run on the same engine as implement.
	fixerModel := strings.TrimSpace(cfg.Model)
	noProgress := 0
	if env := os.Getenv("DEVAGENT_NO_PROGRESS_TIMEOUT_MS"); env != "" {
		if n, perr := strconv.Atoi(env); perr == nil {
			noProgress = n
		}
	}
	result := dispatcher.Dispatch(WorkerDispatchRequest{
		Prompt:              req.Prompt,
		Cwd:                 wt,
		TimeoutMs:           timeoutMs,
		Model:               fixerModel,
		NoProgressTimeoutMs: noProgress,
	})
	if result.TimedOut || result.ExitCode != 0 {
		timedOut := ""
		if result.TimedOut {
			timedOut = ", timed out"
		}
		return CiFixResult{OK: false, Note: fmt.Sprintf("fix worker %s failed (exit %d%s)", workerName, result.ExitCode, timedOut)}
	}

	// Commit everything and push with lease against FETCH_HEAD.
	status := spawn.RunCli("git", []string{"status", "--porcelain"}, spawn.Options{Dir: wt, TimeoutMs: 15_000})
	if status.ExitCode != 0 {
		return CiFixResult{OK: false, Note: "git status failed in fix worktree: " + sliceUTF16(status.Stderr, 120)}
	}
	if strings.TrimSpace(status.Stdout) == "" {
		return CiFixResult{OK: false, Note: "fix worker made no changes — not pushing an empty commit"}
	}
	if _, err := runGit([]string{"add", "-A"}, wt, 60_000); err != nil {
		return fail(err)
	}
	if _, err := runGit([]string{"commit", "-m", fmt.Sprintf("devagent(TASK-fix-%d): fix failing CI checks", req.PR)}, wt, 60_000); err != nil {
		return fail(err)
	}
	if _, err := runGit([]string{"push", "--force-with-lease=FETCH_HEAD", "origin", "HEAD:refs/heads/" + branch}, wt, 60_000); err != nil {
		return fail(err)
	}
	return CiFixResult{OK: true, Note: fmt.Sprintf("fix pushed to %s by %s", branch, workerName)}
}

// AutoReviewAndMergeBatchOpts mirrors the TS autoReviewAndMerge opts (`& {
// prNumbers?, log? }`).
type AutoReviewAndMergeBatchOpts struct {
	AutoReviewAndMergeOptions
	// PrNumbers: explicit --pr sets bypass the supersession view
	// (hand-picked batches override). nil = list all open PRs.
	PrNumbers []int
	Log       func(msg string)
}

// AutoReviewAndMerge mirrors the TS autoReviewAndMerge: batch entry point —
// every open PR matching the filters, oldest first.
func AutoReviewAndMerge(repoPath string, opts AutoReviewAndMergeBatchOpts, run RunGh) []AutoMergeOutcome {
	// Supersession view (full listing only): per base, the lowest-numbered
	// open PR that is a mergeable candidate. Non-candidate siblings on the
	// same base are skipped with a superseded reason instead of parking the
	// queue. Explicit --pr sets bypass the view: a hand-picked batch
	// overrides it.
	var supersedingByBase map[string]int
	baseByNumber := map[int]string{}
	var numbers []int
	if opts.PrNumbers != nil {
		numbers = opts.PrNumbers
	} else {
		statuses, err := ListOpenPrs(repoPath, run)
		if err != nil {
			if opts.Log != nil {
				opts.Log(fmt.Sprintf("cannot list open PRs: %s", err.Error()))
			}
			return []AutoMergeOutcome{}
		}
		sort.Slice(statuses, func(i, j int) bool { return statuses[i].Number < statuses[j].Number })
		supersedingByBase = map[string]int{}
		for _, p := range statuses {
			baseByNumber[p.Number] = p.BaseRefName
			if !isMergeCandidate(p) {
				continue
			}
			if cur, seen := supersedingByBase[p.BaseRefName]; !seen || p.Number < cur {
				supersedingByBase[p.BaseRefName] = p.Number
			}
		}
		numbers = make([]int, 0, len(statuses))
		for _, p := range statuses {
			numbers = append(numbers, p.Number)
		}
	}
	outcomes := []AutoMergeOutcome{}
	for _, n := range numbers {
		var superseding *int
		if base, seen := baseByNumber[n]; seen {
			if c, ok := supersedingByBase[base]; ok {
				superseding = &c
			}
		}
		oneOpts := AutoReviewAndMergeOneOpts{AutoReviewAndMergeOptions: opts.AutoReviewAndMergeOptions, Log: opts.Log}
		oneOpts.SupersedingCandidate = superseding
		outcome := AutoReviewAndMergeOne(repoPath, n, oneOpts, run)
		outcomes = append(outcomes, outcome)
		if opts.Log != nil {
			opts.Log(fmt.Sprintf("%d %s: %s", outcome.PR, outcome.Action, outcome.Detail))
		}
	}
	return outcomes
}

// ZombiePrAction mirrors the TS ZombiePrAction union.
type ZombiePrAction = string

const (
	ZombieActionSuperseded ZombiePrAction = "superseded"
	ZombieActionClosed     ZombiePrAction = "closed"
	ZombieActionSkipped    ZombiePrAction = "skipped"
	ZombieActionUntouched  ZombiePrAction = "untouched"
)

// ZombiePrOutcome mirrors the TS ZombiePrOutcome.
type ZombiePrOutcome struct {
	PR     int            `json:"pr"`
	Title  string         `json:"title"`
	Action ZombiePrAction `json:"action"`
	Detail string         `json:"detail"`
}

// ZombiePrOptions mirrors the TS ZombiePrOptions.
type ZombiePrOptions struct {
	// Days a PR may stay red before auto-close (config zombiePrs.graceDays,
	// default 7). nil = default.
	GraceDays *float64
	// Report without commenting or closing (config zombiePrs.dryRun, default
	// true). nil = default.
	DryRun *bool
	Log    func(msg string)
}

// isMergeCandidate mirrors the TS isMergeCandidate: a mergeable candidate —
// open, mergeable, and provably green (checks completed and passed). PRs
// with pending or missing checks carry no evidence and never count — neither
// as candidates nor as zombies.
func isMergeCandidate(status PrStatus) bool {
	if status.State != "OPEN" || status.Mergeable != "MERGEABLE" {
		return false
	}
	cv := EvaluateChecksOf(status)
	return !cv.Pending && cv.Passed && len(status.Checks) > 0
}

// SweepStalePrs mirrors the TS sweepStalePrs: zombie-PR hygiene sweep
// (PRD.md:737, second clause) — every open PR is a mergeable candidate, left
// untouched, commented as superseded, or closed — per the config zombiePrs
// block (graceDays, dryRun; dry-run default):
//   - superseded: the head SHA is not a mergeable candidate while another
//     open PR on the same base is; the PR gets a skip comment naming the
//     candidate.
//   - closed: CI red (completed failures, nothing pending) across the grace
//     window measured from updatedAt; closed with an explanatory comment.
//   - untouched: everything else (pending or missing checks, green, or red
//     within grace) carries no supersession evidence and stays as-is.
func SweepStalePrs(repoPath string, opts ZombiePrOptions, run RunGh) []ZombiePrOutcome {
	var cfgGraceDays *float64
	var cfgDryRun *bool
	if cfg, err := config.Load(repoPath); err == nil && cfg.ZombiePrs != nil {
		cfgGraceDays = cfg.ZombiePrs.GraceDays
		cfgDryRun = cfg.ZombiePrs.DryRun
	}
	graceDays := 7.0
	if opts.GraceDays != nil {
		graceDays = *opts.GraceDays
	} else if cfgGraceDays != nil {
		graceDays = *cfgGraceDays
	}
	dryRun := true
	if opts.DryRun != nil {
		dryRun = *opts.DryRun
	} else if cfgDryRun != nil {
		dryRun = *cfgDryRun
	}
	logFn := opts.Log
	if logFn == nil {
		logFn = func(string) {}
	}
	prs, err := ListOpenPrs(repoPath, run)
	if err != nil {
		return []ZombiePrOutcome{}
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
	candidateByBase := map[string]int{}
	for _, p := range prs {
		if isMergeCandidate(p) {
			if _, seen := candidateByBase[p.BaseRefName]; !seen {
				candidateByBase[p.BaseRefName] = p.Number
			}
		}
	}
	outcomes := []ZombiePrOutcome{}
	for _, status := range prs {
		if status.State != "OPEN" {
			outcomes = append(outcomes, ZombiePrOutcome{PR: status.Number, Title: status.Title, Action: ZombieActionSkipped, Detail: fmt.Sprintf("state is %s", status.State)})
			continue
		}
		candidate, hasCandidate := candidateByBase[status.BaseRefName]
		cv := EvaluateChecksOf(status)
		if hasCandidate && candidate != status.Number && !cv.Pending && cv.Passed {
			// Not red, not a candidate, and another open PR on the same base
			// is: this head is superseded (conflicting, blocked, or
			// unmergeable).
			head := status.HeadRefOid
			if head == "" {
				head = "unknown-sha"
			}
			detail := fmt.Sprintf("head %s is not a mergeable candidate; base %s superseded by #%d", head, status.BaseRefName, candidate)
			body := strings.Join([]string{
				fmt.Sprintf("DevAgent zombie-PR sweep: skipping this PR — head %s is not a mergeable candidate.", head),
				fmt.Sprintf("Base %s is superseded by open PR #%d; rebase or close this branch.", status.BaseRefName, candidate),
			}, "\n")
			if dryRun {
				outcomes = append(outcomes, ZombiePrOutcome{PR: status.Number, Title: status.Title, Action: ZombieActionSuperseded, Detail: "[dry-run] " + detail})
			} else {
				_ = PostPrComment(repoPath, status.Number, body, run)
				logFn(fmt.Sprintf("#%d superseded: %s", status.Number, detail))
				outcomes = append(outcomes, ZombiePrOutcome{PR: status.Number, Title: status.Title, Action: ZombieActionSuperseded, Detail: detail})
			}
			continue
		}
		if !cv.Pending && !cv.Passed {
			updatedMs, ok := jsDateParse(status.UpdatedAt)
			redDays := math.Inf(1)
			if ok {
				redDays = float64(timeNowMs()-updatedMs) / 86_400_000.0
			}
			if redDays >= graceDays {
				detail := fmt.Sprintf("red across %sd grace window (%dd since last update)", formatHours(graceDays), int(mathFloor(redDays)))
				if dryRun {
					outcomes = append(outcomes, ZombiePrOutcome{PR: status.Number, Title: status.Title, Action: ZombieActionClosed, Detail: "[dry-run] " + detail})
				} else {
					_ = PostPrComment(repoPath, status.Number, strings.Join([]string{
						"DevAgent zombie-PR sweep: auto-closing this PR.",
						fmt.Sprintf("CI has been red for the full %s-day grace window (%s);", formatHours(graceDays), cv.Summary),
						"reopen with a fix or abandon the branch.",
					}, "\n"), run)
					_, _ = run([]string{"pr", "close", strconv.Itoa(status.Number)}, repoPath)
					logFn(fmt.Sprintf("#%d closed: %s", status.Number, detail))
					outcomes = append(outcomes, ZombiePrOutcome{PR: status.Number, Title: status.Title, Action: ZombieActionClosed, Detail: detail})
				}
				continue
			}
			outcomes = append(outcomes, ZombiePrOutcome{PR: status.Number, Title: status.Title, Action: ZombieActionUntouched, Detail: fmt.Sprintf("red within grace: %s", cv.Summary)})
			continue
		}
		detail := fmt.Sprintf("no superseding candidate: %s", cv.Summary)
		if cv.Pending {
			detail = fmt.Sprintf("checks pending: %s", cv.Summary)
		}
		outcomes = append(outcomes, ZombiePrOutcome{PR: status.Number, Title: status.Title, Action: ZombieActionUntouched, Detail: detail})
	}
	return outcomes
}
