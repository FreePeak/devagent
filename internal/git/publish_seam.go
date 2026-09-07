package git

// Publish seam for the task flow (src/task.ts TaskPublishDeps, lines 76-77):
// after a run's attempt branch is committed and its changed-file set is
// non-empty, the pipeline pushes the branch and opens a PR via the `gh` CLI.
// The real implementation lives in src/integrations/github.ts (pushBranch,
// createPr), whose Go port belongs to the integrations package
// (FR-GO-09 #199) — not this one. Until that lands, the git layer defines
// the seam so the publish flow is wireable.
//
// TODO(FR-GO-09 #199): replace with the sibling integrations port.
//
// Parity notes for the future implementation, pinned here so the contract
// survives the handoff:
//   - pushBranch: `git push -u origin <branch>:<branch>` (explicit refspec so
//     local-only branches publish cleanly), cwd = repo root, 120s budget;
//     error message `git push <branch> failed: <stderr>`; wrapped in
//     withRateLimitRetry (one 60s-backoff retry on GitHub rate-limit text).
//   - createPr: `gh pr create -t <title> -b <body> [-B <base>] -H <branch>`;
//     PR URL parsed from the last non-empty stdout line; error message
//     `gh pr create failed for branch "<branch>": <stderr>`; a run that
//     produced no URL fails with `gh pr create produced no PR URL`.
type PRCreateOptions struct {
	// RepoPath is absolute or relative to the git repository.
	RepoPath string
	Branch   string
	Title    string
	Body     string
	// BaseBranch the PR is opened against (defaults to gh's default when empty).
	BaseBranch string
}

// PROpener is the interface-only seam for opening a PR via `gh`. No gh call
// is made from this package.
type PROpener interface {
	CreatePR(opts PRCreateOptions) (url string, err error)
}

// BranchPusher is the matching seam for pushing an attempt branch to origin.
type BranchPusher interface {
	PushBranch(repoPath string, branch string) error
}

// PublishSeam is the pair of operations publishTaskBranch (src/task.ts:99)
// needs from the integrations layer.
type PublishSeam interface {
	BranchPusher
	PROpener
}
