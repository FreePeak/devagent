// Package file mirrors src/orchestrator/merge.ts (FR-GO-07, issue #194).

package orchestrator

import (
	"fmt"
	"strings"

	"github.com/FreePeak/devagent/internal/gates"
	"github.com/FreePeak/devagent/internal/git"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/spawn"
)

// Merge-back: after all tasks are done, integrate each task branch into the
// base branch in dependency order (topological), re-running the test gate on
// the merged tree. A merge conflict or gate failure stops integration with a
// clear report rather than force-continuing (divergence-guard discipline).

// MergeRunner is the CLI seam for the git IO and the integrated-tree test
// gate (TS: the git() helper and runTestGate, both via spawnCli). It is the
// gates.Runner shape (RunCli + spawn.Options) so the test gate runner and
// the merge-back runner are one injectable seam. TODO(FR-GO-03 #195):
// replace with the internal/git port once it exports checkout/merge/reset
// runners.
type MergeRunner = gates.Runner

// mergeGit mirrors the TS git() helper: one git invocation through the
// injected runner (nil → internal/spawn), 60s default timeout.
func mergeGit(runner MergeRunner, args []string, cwd string, timeoutMs int) spawn.Result {
	if timeoutMs == 0 {
		timeoutMs = 60_000
	}
	if runner != nil {
		return runner.RunCli("git", args, spawn.Options{Dir: cwd, TimeoutMs: timeoutMs})
	}
	return spawn.RunCli("git", args, spawn.Options{Dir: cwd, TimeoutMs: timeoutMs})
}

// jsTrim mirrors TS String.prototype.trim().
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0x0085, 0x00A0, 0x1680, 0x2028,
			0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
			return true
		}
		return r >= 0x2000 && r <= 0x200A
	})
}

// sliceUTF16 mirrors TS String.prototype.slice(0, n): n counts UTF-16 code
// units, not bytes or runes. A rune beyond the limit is dropped whole (the
// only divergence from V8, which can emit a lone surrogate — not
// representable in Go strings and never JSON-identical anyway).
func sliceUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

// TopoOrder mirrors topoOrder: topological order over dependsOn (board is
// validated cycle-free at plan time). Dependencies are emitted before
// dependents; board order breaks ties between independent tasks.
func TopoOrder(board ProjectBoard) []string {
	byId := make(map[string]OrchestratorTask, len(board.Tasks))
	for _, t := range board.Tasks {
		byId[t.ID] = t
	}
	order := make([]string, 0, len(board.Tasks))
	seen := make(map[string]bool, len(board.Tasks))
	var visit func(id string)
	visit = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		// byId.get(id)?.dependsOn ?? []: ids missing from the board (already
		// archived dependencies) are still emitted, exactly like the TS Map
		// miss.
		for _, d := range byId[id].DependsOn {
			visit(d)
		}
		order = append(order, id)
	}
	for _, t := range board.Tasks {
		visit(t.ID)
	}
	return order
}

// MergeFailure mirrors the TS MergeResult failure member
// (stage: 'checkout' | 'merge' | 'gate').
type MergeFailure struct {
	TaskID string `json:"taskId"`
	Stage  string `json:"stage"`
	Detail string `json:"detail"`
}

// MergeResult mirrors the TS MergeResult interface.
type MergeResult struct {
	Ok      bool          `json:"ok"`
	Merged  []string      `json:"merged"`
	Failure *MergeFailure `json:"failure,omitempty"`
}

// PerTaskPrPublished mirrors perTaskPrPublished: true when the per-task PR
// flow (PR #71 publishTaskPr) already published at least one done task's
// branch as a PR. In that case the legacy all-done local merge-back must not
// run — it would double-merge branches whose integration is already owned by
// their open PRs (PRD Q20, IMPROVE-retire-legacy-mergeback).
func PerTaskPrPublished(board ProjectBoard) bool {
	for _, t := range board.Tasks {
		if t.Status == TaskStatusDone && t.PrURL != "" {
			return true
		}
	}
	return false
}

// MergeProjectBranches mirrors mergeProjectBranches. TS logs via a RunLogger
// and accepts the runner seam for hermetic tests; a nil runner delegates to
// internal/spawn. A nil log is a no-op.
func MergeProjectBranches(repoPath string, board ProjectBoard, baseBranch string, log RunLog, runner MergeRunner) MergeResult {
	doneIds := make(map[string]bool, len(board.Tasks))
	for _, t := range board.Tasks {
		if t.Status == TaskStatusDone {
			doneIds[t.ID] = true
		}
	}
	// Ensure base exists locally and is checked out in the main worktree
	checkout := mergeGit(runner, []string{"checkout", baseBranch}, repoPath, 60_000)
	if checkout.ExitCode != 0 {
		return MergeResult{
			Ok:     false,
			Merged: []string{},
			Failure: &MergeFailure{
				TaskID: "-",
				Stage:  "checkout",
				Detail: fmt.Sprintf("cannot checkout %s: %s", baseBranch, sliceUTF16(jsTrim(checkout.Stderr), 200)),
			},
		}
	}

	merged := []string{}
	for _, id := range TopoOrder(board) {
		if !doneIds[id] {
			continue
		}
		var task *OrchestratorTask
		for i := range board.Tasks {
			if board.Tasks[i].ID == id {
				task = &board.Tasks[i]
				break
			}
		}
		recoveries := derefInt(task.Recoveries)
		branch := fmt.Sprintf("devagent/%s-%s", git.SanitizeTicketID(id), AttemptSuffix(task.Attempts, recoveries))
		m := mergeGit(runner, []string{"merge", "--no-ff", "--no-edit", branch}, repoPath, 60_000)
		if m.ExitCode != 0 {
			_ = mergeGit(runner, []string{"merge", "--abort"}, repoPath, 60_000) // leave the tree clean
			// git reports conflicts on stdout ("CONFLICT (content): ..."), errors on stderr
			detail := sliceUTF16(jsTrim(jsTrim(m.Stdout)+"\n"+jsTrim(m.Stderr)), 300)
			if detail == "" {
				detail = fmt.Sprintf("merge %s failed", branch)
			}
			return MergeResult{
				Ok:      false,
				Merged:  merged,
				Failure: &MergeFailure{TaskID: id, Stage: "merge", Detail: detail},
			}
		}
		// Gate the integrated tree after every merge
		g1, err := gates.RunTestGate(runner, repoPath, 10*60_000)
		if err != nil {
			// The Go gates port surfaces detectTestCommand failures as an
			// error; the TS version never throws here, so the error is
			// folded into the gate-failed path (rollback + report).
			g1 = gates.GateResult{Passed: false, Detail: err.Error()}
		}
		if !g1.Passed {
			// roll back this merge; earlier merges stay (they passed their gates)
			rollback := mergeGit(runner, []string{"reset", "--hard", "HEAD@{1}"}, repoPath, 60_000)
			detail := ""
			if rollback.ExitCode == 0 {
				gateDetail := "unknown"
				if g1.Detail != "" {
					gateDetail = sliceUTF16(g1.Detail, 200)
				}
				detail = fmt.Sprintf("integrated tests failed: %s", gateDetail)
			} else {
				detail = fmt.Sprintf("tests failed and rollback failed: %s", sliceUTF16(g1.Detail, 150))
			}
			return MergeResult{
				Ok:      false,
				Merged:  merged,
				Failure: &MergeFailure{TaskID: id, Stage: "gate", Detail: detail},
			}
		}
		merged = append(merged, id)
		if log != nil {
			log.Info(ledger.StageTask, fmt.Sprintf("Merged %s into %s", branch, baseBranch), nil)
		}
	}
	return MergeResult{Ok: true, Merged: merged}
}

// RestoreAutoStash mirrors restoreAutoStash (Q26, PRD:927): restore the
// merge-back auto-stash and surface the outcome as a ledger warning. Console
// output is lost when the loop runs unattended, so the merge-back-stash row
// is how operators discover retained stashes via ledger analytics. A failed
// pop leaves the stash intact (user work is never dropped); the row records
// that so recovery is possible after the run ends. Returns whether the stash
// was restored.
func RestoreAutoStash(repoPath string, stashSha string) bool {
	popped := git.PopStashBySha(repoPath, stashSha)
	outcome := "retained"
	detail := "stash pop failed; stash kept for manual recovery"
	if popped {
		outcome = "restored"
		detail = "auto-stash restored after merge-back"
	}
	ledger.AppendStashRecord(repoPath, ledger.StashRecord{
		TS:       ledger.NowISO(),
		Kind:     "event",
		Event:    "merge-back-stash",
		TaskID:   "merge-back",
		Attempt:  1,
		StashSHA: stashSha,
		Outcome:  outcome,
		Detail:   &detail,
	})
	return popped
}
