// Package git is the Go port of src/git/* (FR-GO-03): worktree lifecycle,
// the durable state branch, merge-queue rebase automation, and doc sync.
// Error strings and result shapes mirror the TypeScript originals exactly
// (byte-parity contract); hermetic tests run against temp fixture repos.
package git

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/FreePeak/devagent/internal/spawn"
)

// Default timeout for worktree-scoped git commands: generous enough for
// worktree add/remove but tight enough to surface hung `git index-pack`
// calls; callers override it per command.
const defaultRunTimeoutMs = 30_000

// pushTimeoutMs: network push budget. The 30s worktree budget is too tight
// (mirrors PUSH_TIMEOUT_MS in src/git/worktree.ts and pushBranch's 120s).
const pushTimeoutMs = 120_000

// RunError mirrors the thrown Error in worktree.ts run(): message shape
// `git <args joined> exited <code>: <stderr[:200]>`, with stdout/stderr
// attached so callers can regex the failure text.
type RunError struct {
	Cmd      string
	Args     []string
	ExitCode int
	Stdout   string
	Stderr   string
}

func (e *RunError) Error() string {
	return fmt.Sprintf("%s %s exited %d: %s", e.Cmd, strings.Join(e.Args, " "), e.ExitCode, truncate(e.Stderr, 200))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// run spawns a git command through spawn.RunCli so the child inherits the
// fallback PATH (live-smoke lesson: a parent's minimal PATH produced
// `spawn git ENOENT` for every worktree operation, killing the selfbuild loop
// on loop 50 and tripping the circuit breaker). Non-zero exit / ENOENT is
// translated into a *RunError so the `try { run } catch { return false }`
// call sites keep working.
func run(args []string, cwd string, timeoutMs int) (string, string, error) {
	return runEnv(args, cwd, timeoutMs, nil)
}

// runEnv is run with extra environment variables merged over the base
// environment (GIT_INDEX_FILE, GIT_SSH_COMMAND, ...).
func runEnv(args []string, cwd string, timeoutMs int, env map[string]string) (string, string, error) {
	if timeoutMs <= 0 {
		timeoutMs = defaultRunTimeoutMs
	}
	r := spawn.RunCli("git", args, spawn.Options{Dir: cwd, TimeoutMs: timeoutMs, Env: env})
	if r.ExitCode != 0 {
		return r.Stdout, r.Stderr, &RunError{Cmd: "git", Args: args, ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr}
	}
	return r.Stdout, r.Stderr, nil
}

// WorktreeInfo mirrors the WorktreeInfo interface.
type WorktreeInfo struct {
	WorktreePath string `json:"worktreePath"`
	Branch       string `json:"branch"`
}

var reUnsafeTicket = regexp.MustCompile(`[^A-Za-z0-9\-_]`)

// SanitizeTicketID sanitizes a ticket id so it is safe for branch and
// directory names.
func SanitizeTicketID(ticketID string) string {
	return reUnsafeTicket.ReplaceAllString(ticketID, "")
}

// IsGitRepository is true when repoPath sits inside a git work tree.
func IsGitRepository(repoPath string) bool {
	_, _, err := run([]string{"rev-parse", "--is-inside-work-tree"}, repoPath, defaultRunTimeoutMs)
	return err == nil
}

// CreateWorktree creates a git worktree for the given ticket:
//
//	branch:  devagent/<sanitized ticket id>
//	path:    <repoPath>/.devagent-worktrees/<sanitized ticket id>
//
// Re-runs reuse prior work: when the branch already exists, the existing
// worktree dir is returned as-is, or a new worktree is attached to the
// existing branch.
func CreateWorktree(repoPath, ticketID string) (WorktreeInfo, error) {
	safeID := SanitizeTicketID(ticketID)
	branch := "devagent/" + safeID
	worktreePath := repoPath + "/.devagent-worktrees/" + safeID

	_, stderr, err := run([]string{"worktree", "add", "-b", branch, worktreePath}, repoPath, defaultRunTimeoutMs)
	if err != nil {
		text := stderr
		if text == "" {
			text = err.Error()
		}
		if strings.Contains(text, "already exists") {
			if _, statErr := os.Stat(worktreePath); statErr == nil {
				return WorktreeInfo{WorktreePath: worktreePath, Branch: branch}, nil
			}
			if _, _, addErr := run([]string{"worktree", "add", worktreePath, branch}, repoPath, defaultRunTimeoutMs); addErr != nil {
				// Stale registration (directory removed without
				// `git worktree remove`): prune orphaned metadata and retry once.
				if _, _, pruneErr := run([]string{"worktree", "prune"}, repoPath, defaultRunTimeoutMs); pruneErr != nil {
					return WorktreeInfo{}, pruneErr
				}
				if _, _, retryErr := run([]string{"worktree", "add", worktreePath, branch}, repoPath, defaultRunTimeoutMs); retryErr != nil {
					return WorktreeInfo{}, retryErr
				}
			}
			return WorktreeInfo{WorktreePath: worktreePath, Branch: branch}, nil
		}
		return WorktreeInfo{}, fmt.Errorf("git worktree add failed: %s", strings.TrimSpace(text))
	}

	return WorktreeInfo{WorktreePath: worktreePath, Branch: branch}, nil
}

// Finalize modes (FinalizeWorktreeOptions.mode).
const (
	ModeRemove   = "remove"
	ModePreserve = "preserve"
)

// FinalizeWorktreeOptions mirrors FinalizeWorktreeOptions.
type FinalizeWorktreeOptions struct {
	RepoPath     string
	WorktreePath string
	TicketID     string
	// Mode is 'remove' (snapshot → push → remove) or 'preserve' (keep the
	// tree untouched for inspection / failure debugging).
	Mode string
	// Remote the run branch is pushed to before removal. Default 'origin'.
	// When the remote is not configured the push is skipped, so local-only
	// repos keep the pre-Q29 disposal behaviour.
	Remote string
}

// FinalizeResult mirrors FinalizeResult.
type FinalizeResult struct {
	// Action is 'removed' or 'preserved'.
	Action    string `json:"action"`
	Committed bool   `json:"committed"` // uncommitted changes were snapshotted onto the branch pre-removal
	Pushed    bool   `json:"pushed"`    // run branch reached the remote before removal was attempted
	// Error is present when removal was requested but did not happen (tree
	// left in place). "" means absent.
	Error string `json:"error,omitempty"`
}

// pushRunBranch pushes the run branch from the main repo — repoPath is the
// only cwd guaranteed to outlive the worktree, and the branch ref lives in
// the shared object database, so the push needs no working tree of its own.
//
// Skipped (not failed) when there is no branch to name — a detached HEAD has
// no run branch to persist — or no such remote. Any other failure is
// reported so the caller can hold the worktree instead of stranding the
// snapshot.
func pushRunBranch(repoPath, worktreePath, remote string) (pushed bool, failure string) {
	branch, err := CurrentBranch(worktreePath)
	if err != nil {
		return false, ""
	}
	if _, _, err := run([]string{"remote", "get-url", remote}, repoPath, defaultRunTimeoutMs); err != nil {
		return false, ""
	}
	// Same refspec as `pushBranch` (src/integrations/github.ts), so the
	// publish stage's push is idempotent rather than a competing update.
	if _, _, err := run([]string{"push", "-u", remote, branch + ":" + branch}, repoPath, pushTimeoutMs); err != nil {
		return false, fmt.Sprintf("run branch %s not pushed to %s: %s", branch, remote, err.Error())
	}
	return true, ""
}

// FinalizeRunWorktree mirrors finalizeRunWorktree: post-run disposal of a
// run's worktree (auto-cleanup stage).
//
// 'remove' mode is one commit path: snapshot any uncommitted worker output
// onto the run branch (nothing is ever lost), push that branch to the remote,
// and only then remove the worktree registration and directory. The branch
// itself is kept: it holds the snapshot and stays cheap.
//
// Push-before-removal is the Q29 fix (PRD §18): publish used to be the only
// stage that pushed and its failure is swallowed non-fatal, so a green task's
// snapshot could sit unpushed behind a worktree that had already died.
// Sequencing remote persistence ahead of the worktree's death means a failed
// push leaves the tree recoverable, and publish's later push resolves to a
// no-op.
func FinalizeRunWorktree(opts FinalizeWorktreeOptions) FinalizeResult {
	if opts.Mode == ModePreserve {
		return FinalizeResult{Action: "preserved"}
	}
	committed := false
	// Snapshot is best-effort; removal below still proceeds for a clean tree.
	if ok, err := CommitAllChanges(opts.WorktreePath,
		fmt.Sprintf("devagent(%s): auto-cleanup snapshot", SanitizeTicketID(opts.TicketID))); err == nil {
		committed = ok
	}
	remote := opts.Remote
	if remote == "" {
		remote = "origin"
	}
	pushed, pushErr := pushRunBranch(opts.RepoPath, opts.WorktreePath, remote)
	if pushErr != "" {
		// Atomicity: the worktree only dies once its output is on the remote.
		return FinalizeResult{Action: "preserved", Committed: committed, Pushed: false, Error: pushErr}
	}
	if _, _, err := run([]string{"worktree", "remove", "--force", opts.WorktreePath}, opts.RepoPath, defaultRunTimeoutMs); err != nil {
		return FinalizeResult{Action: "preserved", Committed: committed, Pushed: pushed, Error: err.Error()}
	}
	if _, _, err := run([]string{"worktree", "prune"}, opts.RepoPath, defaultRunTimeoutMs); err != nil {
		return FinalizeResult{Action: "preserved", Committed: committed, Pushed: pushed, Error: err.Error()}
	}
	return FinalizeResult{Action: "removed", Committed: committed, Pushed: pushed}
}

// RemoveWorktree removes a ticket's worktree with `git worktree remove
// --force`. Best-effort: any failure (e.g. already removed) is swallowed.
func RemoveWorktree(repoPath, ticketID string) {
	safeID := SanitizeTicketID(ticketID)
	worktreePath := repoPath + "/.devagent-worktrees/" + safeID
	_, _, _ = run([]string{"worktree", "remove", "--force", worktreePath}, repoPath, defaultRunTimeoutMs)
}

var reNothingToCommit = regexp.MustCompile(`(?i)nothing to commit`)

// CommitAllChanges stages every change (`git add -A`) and commits with
// --no-verify. Returns true when a commit was created, false when the tree
// was clean (nothing-to-commit is tolerated, not an error).
func CommitAllChanges(worktreePath, message string) (bool, error) {
	if _, _, err := run([]string{"add", "-A"}, worktreePath, defaultRunTimeoutMs); err != nil {
		return false, err
	}
	stdout, stderr, err := run([]string{"commit", "--no-verify", "-m", message}, worktreePath, defaultRunTimeoutMs)
	if err != nil {
		if reNothingToCommit.MatchString(stdout + stderr) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CurrentBranch returns the name of the branch checked out in worktreePath —
// ground truth for publishing (never guess a refspec). It fails on a detached
// HEAD, where "branch" is meaningless and pushing would silently do the wrong
// thing.
func CurrentBranch(worktreePath string) (string, error) {
	stdout, _, err := run([]string{"rev-parse", "--abbrev-ref", "HEAD"}, worktreePath, defaultRunTimeoutMs)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(stdout)
	if name == "" || name == "HEAD" {
		return "", fmt.Errorf("detached HEAD in %s: no branch to publish", worktreePath)
	}
	return name, nil
}

// RenameCurrentBranch renames the currently checked-out branch inside a
// worktree.
func RenameCurrentBranch(worktreePath, newBranch string) error {
	_, _, err := run([]string{"branch", "-m", newBranch}, worktreePath, defaultRunTimeoutMs)
	return err
}

// DeleteBranch hard-deletes a branch from the repo. Best-effort: any failure
// (unknown branch, checked out elsewhere) is swallowed.
func DeleteBranch(repoPath, branch string) {
	_, _, _ = run([]string{"branch", "-D", branch}, repoPath, defaultRunTimeoutMs)
}

// ListChangedFiles lists the files changed on this branch vs its merge-base
// with the default branch. When the run's worktree is already gone
// (cleanup=auto snapshot), pass ref = the surviving run branch so the diff is
// computed against that ref instead of the main worktree's HEAD. An empty ref
// means 'HEAD' (the TypeScript default parameter).
func ListChangedFiles(repoPath, baseBranch, ref string) ([]string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	mergeBase, _, err := run([]string{"merge-base", baseBranch, ref}, repoPath, defaultRunTimeoutMs)
	if err != nil {
		return nil, err
	}
	diff, _, err := run([]string{"diff", "--name-only", strings.TrimSpace(mergeBase), ref}, repoPath, defaultRunTimeoutMs)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(diff, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// StashMainWorktree stashes uncommitted changes (including untracked files)
// in the worktree at repoPath. It returns "" when the tree is already clean
// (no empty stash is created); otherwise it returns the concrete stash SHA.
//
// The SHA (not `stash@{0}`) is what callers must pop: stash indices shift
// whenever anything else stashes concurrently, which is exactly how the
// selfbuild automation clobbered work in earlier loops.
func StashMainWorktree(repoPath, message string) (string, error) {
	statusBefore, _, err := run([]string{"status", "--porcelain"}, repoPath, defaultRunTimeoutMs)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(statusBefore) == "" {
		return "", nil
	}

	stashListBefore, _, err := run([]string{"stash", "list", "--format=%H"}, repoPath, defaultRunTimeoutMs)
	if err != nil {
		return "", err
	}
	if _, _, err := run([]string{"stash", "push", "--include-untracked", "-m", message}, repoPath, defaultRunTimeoutMs); err != nil {
		return "", err
	}
	rev, _, err := run([]string{"rev-parse", "-q", "--verify", "stash@{0}"}, repoPath, defaultRunTimeoutMs)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(rev)
	if sha == "" {
		return "", errors.New("git stash push succeeded but stash@{0} could not be resolved")
	}

	if strings.Contains(stashListBefore, sha) {
		return "", fmt.Errorf("stash push created no new stash entry (sha %s already present)", sha)
	}
	return sha, nil
}

// PopStashBySha restores a stash created by StashMainWorktree, addressed by
// its concrete SHA rather than a shifting `stash@{n}` index. `git stash pop`
// rejects raw SHAs, so this applies the commit then drops the exact stash
// entry it still points at. Returns false when the apply fails (conflict,
// missing stash) and leaves the stash intact — never drops user work.
func PopStashBySha(repoPath, sha string) bool {
	if _, _, err := run([]string{"stash", "apply", sha}, repoPath, defaultRunTimeoutMs); err != nil {
		return false
	}
	list, _, err := run([]string{"stash", "list", "--format=%H"}, repoPath, defaultRunTimeoutMs)
	if err != nil {
		return true // apply succeeded; a failed list leaves the stash in place
	}
	entries := make([]string, 0, 8)
	for _, line := range strings.Split(list, "\n") {
		if line != "" {
			entries = append(entries, line)
		}
	}
	idx := -1
	for i, line := range entries {
		if line == sha {
			idx = i
			break
		}
	}
	if idx < 0 {
		return true // already consumed by a concurrent pop
	}
	current, _, err := run([]string{"rev-parse", "-q", "--verify", fmt.Sprintf("stash@{%d}", idx)}, repoPath, defaultRunTimeoutMs)
	if err != nil {
		return true
	}
	if strings.TrimSpace(current) == sha {
		_, _, _ = run([]string{"stash", "drop", fmt.Sprintf("stash@{%d}", idx)}, repoPath, defaultRunTimeoutMs)
	}
	return true
}

// AssertCleanMainWorktree is the fail-fast guard for the merge-back path: it
// refuses to run when the main worktree's HEAD is detached or on a branch
// other than baseBranch, or when it carries uncommitted changes. The branch
// check runs before the status check so the reported error is always the most
// actionable one. An empty baseBranch means 'main' (the TS default).
func AssertCleanMainWorktree(repoPath, baseBranch string) error {
	if baseBranch == "" {
		baseBranch = "main"
	}
	head, _, err := run([]string{"rev-parse", "--abbrev-ref", "HEAD"}, repoPath, defaultRunTimeoutMs)
	if err != nil {
		return err
	}
	branch := strings.TrimSpace(head)
	if branch == "HEAD" {
		return errors.New("main worktree is in detached-HEAD state; refusing to merge")
	}
	if branch != baseBranch {
		return fmt.Errorf("main worktree is on branch %s, expected %s; refusing to merge", branch, baseBranch)
	}
	status, _, err := run([]string{"status", "--porcelain"}, repoPath, defaultRunTimeoutMs)
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		var lines []string
		for _, line := range strings.Split(status, "\n") {
			if line != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) > 20 {
			lines = lines[:20]
		}
		return fmt.Errorf("main worktree has uncommitted changes; refusing to merge:\n%s", strings.Join(lines, "\n"))
	}
	return nil
}
