package git

import (
	"os"
	"strings"
	"testing"
)

// Ports test/rebase-stack.test.ts (vitest): stacked-repo fixtures, conflict
// walk-stop, validation-before-touch, worktree-busy refusal.

// initStackedRepo: repo with main@base, plus stacked branches bottom->top
// each one commit ahead.
func initStackedRepo(t *testing.T) (repo string, branches []string) {
	t.Helper()
	repo = t.TempDir() + "/repo"
	os.MkdirAll(repo, 0o755)
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "test")
	writeFile(t, repo+"/f.txt", "base\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "base")

	// loop60 branches off main and adds its own file
	runGit(t, repo, "checkout", "-b", "devagent/loop60")
	writeFile(t, repo+"/l60.txt", "60\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "loop60 work")

	// loop61 stacks on loop60
	runGit(t, repo, "checkout", "-b", "devagent/loop61")
	writeFile(t, repo+"/l61.txt", "61\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "loop61 work")

	// main moves forward (the "parent landed" event)
	runGit(t, repo, "checkout", "main")
	writeFile(t, repo+"/main.txt", "new\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "main moved")

	return repo, []string{"devagent/loop60", "devagent/loop61"}
}

func TestRebaseStackRebasesDriftedChildrenInOrder(t *testing.T) {
	repo, branches := initStackedRepo(t)
	r := RebaseStack(repo, branches, &RebaseStackOpts{Onto: "main"})
	if !r.OK {
		t.Fatalf("r = %+v", r)
	}
	if len(r.Results) != 2 || r.Results[0].Outcome != OutcomeRebased || r.Results[1].Outcome != OutcomeRebased {
		t.Fatalf("results = %+v", r.Results)
	}
	for _, b := range branches {
		runGit(t, repo, "merge-base", "--is-ancestor", "main", b) // fails the test if not ancestor
		// own work survives the rebase
		if got := runGit(t, repo, "show", b+":l"+b[len(b)-2:]+".txt"); !strings.Contains(got, "6") {
			t.Fatalf("show = %q", got)
		}
	}
	// stack order preserved: loop61 still contains loop60's tip
	runGit(t, repo, "merge-base", "--is-ancestor", branches[0], branches[1])
}

func TestRebaseStackReportsUpToDateWhenNothingDrifted(t *testing.T) {
	repo, branches := initStackedRepo(t)
	RebaseStack(repo, branches, &RebaseStackOpts{Onto: "main"})      // first pass fixes drift
	r := RebaseStack(repo, branches, &RebaseStackOpts{Onto: "main"}) // second is a no-op
	if !r.OK {
		t.Fatalf("r = %+v", r)
	}
	if len(r.Results) != 2 || r.Results[0].Outcome != OutcomeUpToDate || r.Results[1].Outcome != OutcomeUpToDate {
		t.Fatalf("results = %+v", r.Results)
	}
}

func TestRebaseStackStopsAtConflictChildrenUntouched(t *testing.T) {
	repo := t.TempDir() + "/repo"
	os.MkdirAll(repo, 0o755)
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "test")
	writeFile(t, repo+"/shared.txt", "base\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "base")

	runGit(t, repo, "checkout", "-b", "b1")
	writeFile(t, repo+"/shared.txt", "b1 edit\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "b1 work")

	runGit(t, repo, "checkout", "-b", "b2")
	writeFile(t, repo+"/other.txt", "b2\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "b2 work")

	// conflicting change on main to the same line b1 touched
	runGit(t, repo, "checkout", "main")
	writeFile(t, repo+"/shared.txt", "main edit\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "main conflicts with b1")

	r := RebaseStack(repo, []string{"b1", "b2"}, &RebaseStackOpts{Onto: "main"})
	if r.OK {
		t.Fatalf("r = %+v", r)
	}
	if len(r.Results) != 1 { // walk stops at the conflict
		t.Fatalf("results = %+v", r.Results)
	}
	if r.Results[0].Branch != "b1" || r.Results[0].Outcome != OutcomeConflict {
		t.Fatalf("results[0] = %+v", r.Results[0])
	}
	// conflicted branch ref untouched; child still based on old parent
	// (merge-base --is-ancestor exits non-zero when false)
	runGitFail(t, repo, "merge-base", "--is-ancestor", "main", "b1")
	runGitFail(t, repo, "merge-base", "--is-ancestor", "main", "b2")
	// no leftover rebase state in any worktree
	if wt := runGit(t, repo, "worktree", "list"); strings.Contains(wt, "da-rebase-") {
		t.Fatalf("leftover throwaway worktree: %q", wt)
	}
}

func TestRebaseStackRejectsDisconnectedHistoryBeforeTouchingAnything(t *testing.T) {
	repo, _ := initStackedRepo(t)
	// orphan branch shares no ancestry with the stack
	runGit(t, repo, "checkout", "--orphan", "solo")
	writeFile(t, repo+"/orphan.txt", "orphan\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "orphan root")
	runGit(t, repo, "checkout", "main")

	r := RebaseStack(repo, []string{"devagent/loop60", "solo"}, &RebaseStackOpts{Onto: "main"})
	if r.OK {
		t.Fatalf("r = %+v", r)
	}
	if len(r.Results) != 1 || r.Results[0].Branch != "solo" || r.Results[0].Outcome != OutcomeError {
		t.Fatalf("results = %+v", r.Results)
	}
	// validation ran before any rebase: loop60 never moved
	for _, res := range r.Results {
		if res.Branch == "devagent/loop60" {
			t.Fatalf("loop60 must not appear: %+v", r.Results)
		}
	}
}

func TestRebaseStackRefusesBranchCheckedOutInWorktree(t *testing.T) {
	repo, branches := initStackedRepo(t)
	wt := t.TempDir() + "/held"
	runGit(t, repo, "worktree", "add", wt, "devagent/loop60")
	r := RebaseStack(repo, branches, &RebaseStackOpts{Onto: "main"})
	if r.OK {
		t.Fatalf("r = %+v", r)
	}
	res := r.Results[0]
	if res.Branch != "devagent/loop60" || res.Outcome != OutcomeError || !strings.Contains(res.Detail, "checked out") {
		t.Fatalf("results[0] = %+v", res)
	}
}

func TestRebaseStackErrorsOnUnknownBranchOrMissingOnto(t *testing.T) {
	repo, _ := initStackedRepo(t)
	if r := RebaseStack(repo, []string{"nope"}, &RebaseStackOpts{}); r.Results[0].Outcome != OutcomeError {
		t.Fatalf("unknown branch: %+v", r)
	}
	if r := RebaseStack(repo, []string{"devagent/loop60"}, &RebaseStackOpts{Onto: "ghost"}); r.Results[0].Outcome != OutcomeError {
		t.Fatalf("missing onto: %+v", r)
	}
}

func TestRebaseStackEmptyStackIsNoOpSuccess(t *testing.T) {
	repo, _ := initStackedRepo(t)
	r := RebaseStack(repo, []string{}, &RebaseStackOpts{Onto: "main"})
	if !r.OK || len(r.Results) != 0 {
		t.Fatalf("r = %+v", r)
	}
}
