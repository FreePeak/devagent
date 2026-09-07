package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ports test/clean-main-worktree.test.ts (vitest) — the stash-by-SHA and
// clean-main-worktree guard matrix (clean / dirty / detached / wrong-branch),
// coverage added with PR #84.

func porcelain(t *testing.T, repo string) string {
	t.Helper()
	return runGit(t, repo, "status", "--porcelain")
}

func TestStashMainWorktreeNullOnCleanTree(t *testing.T) {
	repo := initRepo(t)
	sha, err := StashMainWorktree(repo, "test")
	if err != nil {
		t.Fatal(err)
	}
	if sha != "" {
		t.Fatalf("sha = %q, want empty", sha)
	}
	if list := runGit(t, repo, "stash", "list"); list != "" {
		t.Fatalf("stash list = %q", list)
	}
}

func TestStashMainWorktreeStashAndPopBySha(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, filepath.Join(repo, "f.txt"), "modified\n")
	writeFile(t, filepath.Join(repo, "untracked.txt"), "new\n")
	if porcelain(t, repo) == "" {
		t.Fatal("tree should be dirty")
	}

	sha, err := StashMainWorktree(repo, "devagent auto-stash before merge")
	if err != nil {
		t.Fatal(err)
	}
	if len(sha) != 40 {
		t.Fatalf("sha = %q", sha)
	}
	if porcelain(t, repo) != "" {
		t.Fatalf("tree should be clean after stash: %q", porcelain(t, repo))
	}

	if !PopStashBySha(repo, sha) {
		t.Fatal("pop should succeed")
	}
	if data, err := os.ReadFile(filepath.Join(repo, "f.txt")); err != nil || string(data) != "modified\n" {
		t.Fatalf("f.txt = %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "untracked.txt")); err != nil || string(data) != "new\n" {
		t.Fatalf("untracked.txt = %q, %v", data, err)
	}
	// pop consumed the stash entry: no stash refs remain
	if list := runGit(t, repo, "stash", "list"); list != "" {
		t.Fatalf("stash list = %q", list)
	}
}

func TestPopStashByShaFalseOnConflictLeavesStashIntact(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, filepath.Join(repo, "f.txt"), "stashed version\n")
	sha, err := StashMainWorktree(repo, "will conflict")
	if err != nil || sha == "" {
		t.Fatalf("stash = %q, %v", sha, err)
	}
	// Conflicting uncommitted change: apply fails, stash must survive.
	writeFile(t, filepath.Join(repo, "f.txt"), "divergent\n")
	if PopStashBySha(repo, sha) {
		t.Fatal("pop should report failure on conflict")
	}
	if list := runGit(t, repo, "stash", "list", "--format=%H"); !strings.Contains(list, sha) {
		t.Fatalf("stash must remain intact, list = %q", list)
	}
}

func TestAssertCleanMainWorktreeResolvesOnCleanMain(t *testing.T) {
	repo := initRepo(t)
	if err := AssertCleanMainWorktree(repo, "main"); err != nil {
		t.Fatalf("clean main should pass: %v", err)
	}
}

func TestAssertCleanMainWorktreeRejectsDetachedHead(t *testing.T) {
	repo := initRepo(t)
	runGit(t, repo, "checkout", "--detach")
	err := AssertCleanMainWorktree(repo, "main")
	if err == nil || err.Error() != "main worktree is in detached-HEAD state; refusing to merge" {
		t.Fatalf("err = %v", err)
	}
}

func TestAssertCleanMainWorktreeRejectsWrongBranch(t *testing.T) {
	repo := initRepo(t)
	runGit(t, repo, "checkout", "-b", "feature")
	err := AssertCleanMainWorktree(repo, "main")
	want := "main worktree is on branch feature, expected main; refusing to merge"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
}

func TestAssertCleanMainWorktreeRejectsDirtyTreeWithPreview(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, filepath.Join(repo, "f.txt"), "dirty\n")
	err := AssertCleanMainWorktree(repo, "main")
	if err == nil {
		t.Fatal("dirty tree must be rejected")
	}
	if !strings.HasPrefix(err.Error(), "main worktree has uncommitted changes; refusing to merge:\n") {
		t.Fatalf("err = %q", err.Error())
	}
	if !strings.Contains(err.Error(), "M f.txt") {
		t.Fatalf("porcelain preview missing: %q", err.Error())
	}
}

// ensureStateBranch fixture parity: the state-branch tests live in
// state_branch_test.go; the shared fixture lives in helpers_test.go.
var _ = filepath.Join
