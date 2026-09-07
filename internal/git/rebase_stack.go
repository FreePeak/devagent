package git

import (
	"fmt"
	"os"
	"strings"

	"github.com/FreePeak/devagent/internal/spawn"
)

// Merge-queue rebase automation (PRD Phase 4): stacked loop branches drift
// behind main as parents land, and every refresh used to be a manual
// `git rebase` dance per branch.

// RebaseOutcome values (mirrors the RebaseOutcome union type).
const (
	OutcomeUpToDate = "up-to-date"
	OutcomeRebased  = "rebased"
	OutcomePushed   = "pushed"
	OutcomeConflict = "conflict"
	OutcomeError    = "error"
)

// StackBranchResult mirrors StackBranchResult.
type StackBranchResult struct {
	Branch  string `json:"branch"`
	Outcome string `json:"outcome"`
	// Detail is omitted when empty.
	Detail string `json:"detail,omitempty"`
}

// RebaseStackResult mirrors RebaseStackResult.
type RebaseStackResult struct {
	OK      bool                `json:"ok"`
	Results []StackBranchResult `json:"results"`
}

// RebaseStackOpts mirrors the opts parameter of rebaseStack.
type RebaseStackOpts struct {
	// Onto is the base branch. Default 'main'.
	Onto string
	// Push pushes each rebased branch to origin with a force-with-lease
	// pinned to the pre-rebase tip.
	Push bool
}

// gitExit is the exit-code-preserving spawn shape rebase-stack uses (mirrors
// the git() helper in src/git/rebase-stack.ts): unlike run(), a non-zero
// exit is data, not an error.
type gitExit struct {
	exitCode int
	stdout   string
	stderr   string
}

func gitOut(args []string, cwd string, timeoutMs int) gitExit {
	if timeoutMs <= 0 {
		timeoutMs = 60_000
	}
	r := spawn.RunCli("git", args, spawn.Options{Dir: cwd, TimeoutMs: timeoutMs})
	return gitExit{exitCode: r.ExitCode, stdout: r.Stdout, stderr: r.Stderr}
}

// checkedOutBranches: refs currently checked out in any worktree (a rebase
// would fork them).
func checkedOutBranches(repoPath string) map[string]bool {
	r := gitOut([]string{"worktree", "list", "--porcelain"}, repoPath, 60_000)
	out := map[string]bool{}
	for _, line := range strings.Split(r.stdout, "\n") {
		if strings.HasPrefix(line, "branch ") {
			name := strings.TrimSpace(strings.TrimPrefix(line, "branch "))
			name = strings.TrimPrefix(name, "refs/heads/")
			out[name] = true
		}
	}
	return out
}

// RebaseStack mirrors rebaseStack: walks a stack bottom-up and rebases each
// branch onto its updated parent inside a throwaway detached worktree, so
// the caller's checkout and any task worktrees are never touched.
//
// Divergence-guard discipline (mirrors orchestrator/merge.ts): a conflict
// stops the walk with a clear report instead of force-continuing — children
// of a conflicted branch are left untouched because their parent just moved.
func RebaseStack(repoPath string, branches []string, opts *RebaseStackOpts) RebaseStackResult {
	onto := "main"
	push := false
	if opts != nil {
		if opts.Onto != "" {
			onto = opts.Onto
		}
		push = opts.Push
	}
	results := []StackBranchResult{}
	if len(branches) == 0 {
		return RebaseStackResult{OK: true, Results: results}
	}

	// Validate before touching anything: all branches exist, form a chain over
	// the base, and none is checked out in a worktree (a rebased ref would
	// silently diverge from the checked-out copy).
	for _, b := range branches {
		v := gitOut([]string{"rev-parse", "--verify", "--quiet", b + "^{commit}"}, repoPath, 60_000)
		if v.exitCode != 0 {
			return RebaseStackResult{OK: false, Results: []StackBranchResult{{Branch: b, Outcome: OutcomeError, Detail: "branch not found"}}}
		}
	}
	baseCheck := gitOut([]string{"rev-parse", "--verify", "--quiet", onto + "^{commit}"}, repoPath, 60_000)
	if baseCheck.exitCode != 0 {
		return RebaseStackResult{OK: false, Results: []StackBranchResult{{Branch: onto, Outcome: OutcomeError, Detail: "onto branch not found"}}}
	}
	chain := append([]string{onto}, branches...)
	for i := 0; i < len(branches); i++ {
		// Shared ancestry, not containment: a stacked child forked from its
		// parent's pre-rebase tip, so the moved parent ref cannot be an
		// ancestor of the child anymore.
		base := chain[i]
		branch := branches[i]
		a := gitOut([]string{"merge-base", base, branch}, repoPath, 60_000)
		if a.exitCode != 0 {
			return RebaseStackResult{OK: false, Results: []StackBranchResult{{
				Branch: branch, Outcome: OutcomeError,
				Detail: fmt.Sprintf("not stacked on %s (no common ancestry)", base),
			}}}
		}
	}
	busy := checkedOutBranches(repoPath)
	for _, b := range branches {
		if busy[b] {
			return RebaseStackResult{OK: false, Results: []StackBranchResult{{
				Branch: b, Outcome: OutcomeError,
				Detail: "checked out in a worktree; run from a workspace that does not hold it",
			}}}
		}
	}

	parent := onto
	for _, branch := range branches {
		tipBefore := strings.TrimSpace(gitOut([]string{"rev-parse", branch}, repoPath, 60_000).stdout)
		// Up-to-date when the parent tip is already an ancestor of the branch.
		fresh := gitOut([]string{"merge-base", "--is-ancestor", parent, branch}, repoPath, 60_000)
		if fresh.exitCode == 0 {
			results = append(results, StackBranchResult{Branch: branch, Outcome: OutcomeUpToDate})
			parent = branch
			continue
		}
		if rebaseOneBranch(repoPath, branch, parent, tipBefore, push, &results) {
			return RebaseStackResult{OK: false, Results: results}
		}
		parent = branch
	}
	return RebaseStackResult{OK: true, Results: results}
}

// rebaseOneBranch rebases a single branch inside a throwaway detached
// worktree and appends its result. It returns true (stop) mirroring the TS
// early return: worktree-add failure, conflict, or push failure halts the
// walk — children of the failed branch are left untouched because their
// parent just moved.
func rebaseOneBranch(repoPath, branch, parent, tipBefore string, push bool, results *[]StackBranchResult) bool {
	tmp, err := os.MkdirTemp("", "da-rebase-")
	if err == nil {
		// Rebase in a throwaway detached worktree so no live checkout moves.
		defer func() {
			os.RemoveAll(tmp)
			gitOut([]string{"worktree", "prune"}, repoPath, 60_000)
		}()
		add := gitOut([]string{"worktree", "add", "--detach", tmp, branch}, repoPath, 60_000)
		if add.exitCode == 0 {
			rb := gitOut([]string{"rebase", parent}, tmp, 60_000)
			if rb.exitCode == 0 {
				move := gitOut([]string{"branch", "-f", branch, "HEAD"}, tmp, 60_000)
				if move.exitCode == 0 {
					res := StackBranchResult{Branch: branch, Outcome: OutcomeRebased}
					if push {
						res.Outcome, res.Detail = pushRebasedBranch(repoPath, branch, tipBefore)
					}
					*results = append(*results, res)
					return res.Outcome == OutcomeError
				}
				*results = append(*results, StackBranchResult{Branch: branch, Outcome: OutcomeError,
					Detail: fmt.Sprintf("ref update failed: %s", first200(move.stderr, move.stdout))})
				return true
			}
			gitOut([]string{"rebase", "--abort"}, tmp, 60_000) // leave the tree clean; branch ref untouched
			*results = append(*results, StackBranchResult{Branch: branch, Outcome: OutcomeConflict,
				Detail: fmt.Sprintf("rebase onto %s conflicts; resolve manually (children untouched)", parent)})
			return true
		}
		*results = append(*results, StackBranchResult{Branch: branch, Outcome: OutcomeError,
			Detail: fmt.Sprintf("worktree add failed: %s", first200(add.stderr, add.stdout))})
		return true
	}
	*results = append(*results, StackBranchResult{Branch: branch, Outcome: OutcomeError, Detail: err.Error()})
	return true
}

// pushRebasedBranch pushes a rebased branch and returns the final outcome
// plus an optional failure detail.
func pushRebasedBranch(repoPath, branch, tipBefore string) (outcome string, detail string) {
	// Lease pins the expected remote sha to the pre-rebase tip: if the remote
	// moved while we rebased (another session refreshed the same PR), the
	// push refuses instead of clobbering their update. The explicit-sha form
	// works even with no stale remote-tracking ref.
	pushed := gitOut([]string{"push", "--force-with-lease=refs/heads/" + branch + ":" + tipBefore,
		"origin", branch + ":" + branch}, repoPath, 60_000)
	if pushed.exitCode != 0 {
		return OutcomeError, fmt.Sprintf("push failed: %s", first200(pushed.stderr, pushed.stdout))
	}
	return OutcomePushed, ""
}

func first200(stderr, stdout string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		s = strings.TrimSpace(stdout)
	}
	return truncate(s, 200)
}
