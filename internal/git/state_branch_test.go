package git

import (
	"strings"
	"testing"
)

// Ports test/state-branch.test.ts (vitest). All pushes go to a local bare
// path — no network.

func lsRemoteStateRef(t *testing.T, repo string) string {
	t.Helper()
	return runGit(t, repo, "ls-remote", "--heads", "origin", "selfbuild/state")
}

func seedLessons(t *testing.T, repo, lessons string) {
	t.Helper()
	writeFile(t, repo+"/.devagent/lessons.md", lessons)
	// Track the lessons file so the worktree starts clean and the
	// status-unchanged assertion below is meaningful.
	runGit(t, repo, "add", ".devagent")
	runGit(t, repo, "commit", "-m", "lessons")
}

func TestEnsureStateBranchCreatesOrphanSeededWithLessons(t *testing.T) {
	repo, bare := initFixture(t)
	lessons := "lesson one: verify before claiming completion\n"
	seedLessons(t, repo, lessons)

	r, err := EnsureStateBranch(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Action != "created" {
		t.Fatalf("r = %+v", r)
	}

	// Ref present on the remote.
	ls := lsRemoteStateRef(t, repo)
	if !strings.Contains(ls, "refs/heads/selfbuild/state") {
		t.Fatalf("ls-remote = %q", ls)
	}
	tip := strings.Fields(ls)[0]

	// Branch contains the lessons file with the local content.
	if got := runGit(t, bare, "--git-dir", bare, "show", "refs/heads/selfbuild/state:.devagent/lessons.md"); got != strings.TrimSuffix(lessons, "\n") {
		t.Fatalf("remote lessons = %q", got)
	}
	// Commit is parentless (orphan): rev-list --parents -n1 has exactly one token.
	parents := runGit(t, repo, "rev-list", "--parents", "-n", "1", tip)
	if len(strings.Fields(parents)) != 1 {
		t.Fatalf("parents = %q", parents)
	}
}

func TestEnsureStateBranchLeavesHeadAndWorktreeUntouched(t *testing.T) {
	repo, _ := initFixture(t)
	seedLessons(t, repo, "lesson one: verify before claiming completion\n")

	headBefore := runGit(t, repo, "rev-parse", "HEAD")
	statusBefore := runGit(t, repo, "status", "--porcelain")

	if _, err := EnsureStateBranch(repo, nil); err != nil {
		t.Fatal(err)
	}

	// Work repo untouched.
	if runGit(t, repo, "rev-parse", "HEAD") != headBefore {
		t.Fatal("HEAD moved")
	}
	if runGit(t, repo, "status", "--porcelain") != statusBefore {
		t.Fatal("status changed")
	}
}

func TestEnsureStateBranchEmptyLessonsWhenNoLocalFile(t *testing.T) {
	repo, bare := initFixture(t)
	if fileExists(repo + "/.devagent/lessons.md") {
		t.Fatal("precondition: no local lessons file")
	}

	if _, err := EnsureStateBranch(repo, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lsRemoteStateRef(t, repo), "refs/heads/selfbuild/state") {
		t.Fatal("remote ref missing")
	}
	if got := runGit(t, bare, "--git-dir", bare, "show", "refs/heads/selfbuild/state:.devagent/lessons.md"); got != "" {
		t.Fatalf("remote lessons = %q, want empty", got)
	}
}

func TestEnsureStateBranchNoOpWhenRemoteBranchExists(t *testing.T) {
	repo, bare := initFixture(t)
	writeFile(t, repo+"/.devagent/lessons.md", "seed\n")
	if _, err := EnsureStateBranch(repo, nil); err != nil {
		t.Fatal(err)
	}
	tipBefore := runGit(t, bare, "--git-dir", bare, "rev-parse", "refs/heads/selfbuild/state")

	r, err := EnsureStateBranch(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Action != "exists" {
		t.Fatalf("r = %+v", r)
	}
	if runGit(t, bare, "--git-dir", bare, "rev-parse", "refs/heads/selfbuild/state") != tipBefore {
		t.Fatal("remote tip moved on no-op run")
	}
}
