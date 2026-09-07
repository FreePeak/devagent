package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BoundedGitSSHCommand is the bounded ssh invocation the selfbuild driver
// wraps every network git op in (scripts/selfbuild-loop.sh:100/109):
// BatchMode fails fast instead of prompting for credentials, and
// ConnectTimeout=10 keeps a dead network from hanging the loop. The Go port
// applies it directly at the state-branch network call sites so the
// guarantee travels with the code instead of relying on a shell wrapper.
const BoundedGitSSHCommand = "ssh -o BatchMode=yes -o ConnectTimeout=10"

// EnsureStateBranchOpts mirrors EnsureStateBranchOpts.
type EnsureStateBranchOpts struct {
	// Remote to check and push to. Default 'origin'.
	Remote string
	// Branch to create on the remote. Default 'selfbuild/state'.
	Branch string
	// Repo-relative lessons file to seed the branch with. Default
	// '.devagent/lessons.md'.
	LessonsFile string
}

// EnsureStateBranchResult mirrors EnsureStateBranchResult.
type EnsureStateBranchResult struct {
	// Action is 'created' or 'exists'.
	Action string `json:"action"`
}

// EnsureStateBranch ensures the durable-state branch exists on the remote,
// seeding it with the lessons file on first creation.
//
// Why plumbing-only: the selfbuild automation races on HEAD switches, so
// porcelain orphan-branch workflows (checkout/switch to a new parentless
// root) would clobber the live worktree and are the main failure mode this
// module must avoid. The orphan commit is instead built in a temporary index
// file (GIT_INDEX_FILE) and committed with `git commit-tree` with no parent,
// leaving the current HEAD, index, and working tree completely untouched.
//
// The existence check is remote-side (`git ls-remote`) because the contract
// is about the upstream branch, not any local ref. No fetch is performed.
//
// Network semantics: both network commands (ls-remote, push) carry the
// bounded GIT_SSH_COMMAND and a hard wall-clock timeout (30s check / 120s
// push). A failed push is returned as an error, never retried forever — the
// caller absorbs it and the publish is deferred to the next run ("[state]
// push deferred" in the driver).
func EnsureStateBranch(repoPath string, opts *EnsureStateBranchOpts) (EnsureStateBranchResult, error) {
	var o EnsureStateBranchOpts
	if opts != nil {
		o = *opts
	}
	remote := o.Remote
	if remote == "" {
		remote = "origin"
	}
	branch := o.Branch
	if branch == "" {
		branch = "selfbuild/state"
	}
	lessonsFile := o.LessonsFile
	if lessonsFile == "" {
		lessonsFile = ".devagent/lessons.md"
	}
	sshEnv := map[string]string{"GIT_SSH_COMMAND": BoundedGitSSHCommand}

	// Remote-side existence check: any ref line means the branch already has
	// an upstream tip, so this is a no-op.
	ls, _, err := runEnv([]string{"ls-remote", "--heads", remote, branch}, repoPath, 30_000, sshEnv)
	if err != nil {
		return EnsureStateBranchResult{}, err
	}
	if strings.TrimSpace(ls) != "" {
		return EnsureStateBranchResult{Action: "exists"}, nil
	}

	// Build the orphan commit in a temporary index so the caller's index is
	// never modified.
	tmp, err := os.MkdirTemp("", "da-statebranch-")
	if err != nil {
		return EnsureStateBranchResult{}, err
	}
	defer os.RemoveAll(tmp)
	indexFile := filepath.Join(tmp, "index")

	if _, _, err := runEnv([]string{"read-tree", "--empty"}, repoPath, 30_000, map[string]string{"GIT_INDEX_FILE": indexFile}); err != nil {
		return EnsureStateBranchResult{}, err
	}

	// Seed the lessons file: real content when present locally, empty blob
	// otherwise, so the branch always contains the file.
	localPath := filepath.Join(repoPath, lessonsFile)
	var content []byte
	if data, readErr := os.ReadFile(localPath); readErr == nil && len(data) > 0 {
		content = data
	}
	blobFile := filepath.Join(tmp, "blob")
	if err := os.WriteFile(blobFile, content, 0o644); err != nil {
		return EnsureStateBranchResult{}, err
	}
	blobOut, _, err := run([]string{"hash-object", "-w", blobFile}, repoPath, 30_000)
	if err != nil {
		return EnsureStateBranchResult{}, err
	}
	blobSha := strings.TrimSpace(blobOut)

	if _, _, err := runEnv([]string{"update-index", "--add", "--cacheinfo", fmt.Sprintf("100644,%s,%s", blobSha, lessonsFile)},
		repoPath, 30_000, map[string]string{"GIT_INDEX_FILE": indexFile}); err != nil {
		return EnsureStateBranchResult{}, err
	}

	treeOut, _, err := runEnv([]string{"write-tree"}, repoPath, 30_000, map[string]string{"GIT_INDEX_FILE": indexFile})
	if err != nil {
		return EnsureStateBranchResult{}, err
	}
	treeSha := strings.TrimSpace(treeOut)

	commitOut, _, err := run([]string{"commit-tree", treeSha, "-m", "chore: seed selfbuild/state durable branch"}, repoPath, 30_000)
	if err != nil {
		return EnsureStateBranchResult{}, err
	}
	commitSha := strings.TrimSpace(commitOut)

	// Exactly one push, no -f: the branch was just verified missing, so the
	// ref cannot have moved underneath us unless someone else created it
	// concurrently; in that rare race the push fails and the caller's
	// try/catch-and-continue handles it.
	if _, _, err := runEnv([]string{"push", remote, fmt.Sprintf("%s:refs/heads/%s", commitSha, branch)}, repoPath, 120_000, sshEnv); err != nil {
		return EnsureStateBranchResult{}, err
	}

	return EnsureStateBranchResult{Action: "created"}, nil
}
