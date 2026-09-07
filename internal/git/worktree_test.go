package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ports test/worktree.test.ts (vitest) onto Go table-free behavioral tests
// against the same temp-fixture repos.

func TestCreateWorktreeFreshTicket(t *testing.T) {
	repo := initRepo(t)
	info, err := CreateWorktree(repo, "ENG-1")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(repo, ".devagent-worktrees", "ENG-1")
	if info.WorktreePath != wantPath || info.Branch != "devagent/ENG-1" {
		t.Fatalf("info = %+v", info)
	}
	if !fileExists(info.WorktreePath) {
		t.Fatal("worktree dir missing")
	}
	if got := runGit(t, info.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD"); got != "devagent/ENG-1" {
		t.Fatalf("checked-out branch = %q", got)
	}
}

func TestCreateWorktreeReusesExistingOnRerun(t *testing.T) {
	repo := initRepo(t)
	first, err := CreateWorktree(repo, "ENG-2")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate prior work landing on the branch between runs.
	writeFile(t, filepath.Join(first.WorktreePath, "progress.txt"), "wip\n")
	runGit(t, first.WorktreePath, "add", ".")
	runGit(t, first.WorktreePath, "commit", "-m", "wip")

	second, err := CreateWorktree(repo, "ENG-2")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second = %+v, want %+v", second, first)
	}
	// Prior work survives the re-run.
	if !fileExists(filepath.Join(second.WorktreePath, "progress.txt")) {
		t.Fatal("prior work lost on re-run")
	}
}

func TestCreateWorktreeAttachesWhenDirRemoved(t *testing.T) {
	repo := initRepo(t)
	first, err := CreateWorktree(repo, "ENG-3")
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "worktree", "remove", first.WorktreePath)
	if fileExists(first.WorktreePath) {
		t.Fatal("worktree dir still present after remove")
	}

	second, err := CreateWorktree(repo, "ENG-3")
	if err != nil {
		t.Fatal(err)
	}
	if second.Branch != "devagent/ENG-3" || second.WorktreePath != first.WorktreePath {
		t.Fatalf("second = %+v", second)
	}
	if !fileExists(second.WorktreePath) {
		t.Fatal("re-attached worktree dir missing")
	}
	if got := runGit(t, second.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD"); got != "devagent/ENG-3" {
		t.Fatalf("checked-out branch = %q", got)
	}
}

func TestCommitAllChangesCommitsUntrackedAndModified(t *testing.T) {
	repo := initRepo(t)
	wt, err := CreateWorktree(repo, "ENG-9")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.WorktreePath, "new.txt"), "added\n")
	writeFile(t, filepath.Join(wt.WorktreePath, "f.txt"), "edited\n")

	created, err := CommitAllChanges(wt.WorktreePath, "wip commit")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("commit should be created")
	}
	if status := runGit(t, wt.WorktreePath, "status", "--porcelain"); status != "" {
		t.Fatalf("tree not clean: %q", status)
	}
	if subject := runGit(t, wt.WorktreePath, "log", "-1", "--format=%s"); subject != "wip commit" {
		t.Fatalf("subject = %q", subject)
	}
}

func TestCommitAllChangesToleratesNothingToCommit(t *testing.T) {
	repo := initRepo(t)
	wt, err := CreateWorktree(repo, "ENG-11")
	if err != nil {
		t.Fatal(err)
	}
	created, err := CommitAllChanges(wt.WorktreePath, "should not appear")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("clean tree must not create a commit")
	}
	if subject := runGit(t, wt.WorktreePath, "log", "-1", "--format=%s"); subject != "init" {
		t.Fatalf("subject = %q", subject)
	}
}

func TestRenameCurrentBranchAndDeleteBranchBestEffort(t *testing.T) {
	repo := initRepo(t)
	wt, err := CreateWorktree(repo, "ENG-12")
	if err != nil {
		t.Fatal(err)
	}
	if err := RenameCurrentBranch(wt.WorktreePath, "devagent/ENG-12-canonical"); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, wt.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD"); got != "devagent/ENG-12-canonical" {
		t.Fatalf("current = %q", got)
	}

	// Checked-out branch cannot be deleted from the repo root: must not panic.
	DeleteBranch(repo, "devagent/ENG-12-canonical")
	runGit(t, repo, "branch", "spare")
	DeleteBranch(repo, "spare")
	if branches := runGit(t, repo, "branch", "--list"); strings.Contains(branches, "spare") {
		t.Fatalf("spare should be deleted: %q", branches)
	}
	// Unknown branch: swallowed.
	DeleteBranch(repo, "nope")
}

func TestFinalizeRunWorktreePushesSnapshotBeforeRemoval(t *testing.T) {
	repo, remoteDir := initRepoWithRemote(t)
	wt, err := CreateWorktree(repo, "PUSH-1")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.WorktreePath, "wip.txt"), "uncommitted worker output\n")

	fin := FinalizeRunWorktree(FinalizeWorktreeOptions{
		RepoPath: repo, WorktreePath: wt.WorktreePath, TicketID: "PUSH-1", Mode: ModeRemove,
	})
	if fin.Action != "removed" || !fin.Committed || !fin.Pushed || fin.Error != "" {
		t.Fatalf("fin = %+v", fin)
	}
	if fileExists(wt.WorktreePath) {
		t.Fatal("worktree should be removed")
	}
	// Remote persistence precedes removal: the remote holds exactly the
	// snapshot commit, not a stale pre-run tip.
	if got := remoteTip(remoteDir, wt.Branch); got != runGit(t, repo, "rev-parse", "--verify", wt.Branch) {
		t.Fatalf("remote tip %q != local %q", got, runGit(t, repo, "rev-parse", "--verify", wt.Branch))
	}
	if subject := runGit(t, remoteDir, "log", "-1", "--format=%s", wt.Branch); !strings.Contains(subject, "auto-cleanup snapshot") {
		t.Fatalf("snapshot subject = %q", subject)
	}
	// publish's later push is idempotent: same refspec, nothing to update
	runGit(t, repo, "push", "-u", "origin", wt.Branch+":"+wt.Branch)
	if got := remoteTip(remoteDir, wt.Branch); got != runGit(t, repo, "rev-parse", "--verify", wt.Branch) {
		t.Fatal("idempotent re-push moved the ref")
	}
}

// Bounded-network semantics: push failure is absorbed (reported, not thrown)
// and disposal is deferred — the worktree and its output survive.
func TestFinalizeRunWorktreeHoldsWorktreeWhenPushFails(t *testing.T) {
	repo, base := initRepoWithRemote(t)
	wt, err := CreateWorktree(repo, "PUSH-2")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.WorktreePath, "wip.txt"), "uncommitted worker output\n")
	// Unreachable origin: the exact failure class publish swallows as non-fatal
	runGit(t, repo, "remote", "set-url", "origin", filepath.Join(base, "missing.git"))

	fin := FinalizeRunWorktree(FinalizeWorktreeOptions{
		RepoPath: repo, WorktreePath: wt.WorktreePath, TicketID: "PUSH-2", Mode: ModeRemove,
	})
	if fin.Action != "preserved" || !fin.Committed || fin.Pushed {
		t.Fatalf("fin = %+v", fin)
	}
	if !strings.Contains(fin.Error, "not pushed") {
		t.Fatalf("error = %q", fin.Error)
	}
	if remoteTip(filepath.Join(base, "origin.git"), wt.Branch) != "" {
		t.Fatal("remote should not hold the branch")
	}
	// Still recoverable: the tree and its output survive the failed push
	if !fileExists(wt.WorktreePath) {
		t.Fatal("worktree must survive a failed push")
	}
	if data, err := os.ReadFile(filepath.Join(wt.WorktreePath, "wip.txt")); err != nil || !strings.Contains(string(data), "uncommitted") {
		t.Fatalf("wip.txt = %q, %v", data, err)
	}
}

func TestFinalizeRunWorktreePersistsWorkerCommits(t *testing.T) {
	repo, remoteDir := initRepoWithRemote(t)
	wt, err := CreateWorktree(repo, "PUSH-3")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.WorktreePath, "work.txt"), "committed by the worker\n")
	if _, err := CommitAllChanges(wt.WorktreePath, "worker commit"); err != nil {
		t.Fatal(err)
	}

	fin := FinalizeRunWorktree(FinalizeWorktreeOptions{
		RepoPath: repo, WorktreePath: wt.WorktreePath, TicketID: "PUSH-3", Mode: ModeRemove,
	})
	// Nothing left to snapshot, yet the unpushed branch is still persisted
	if fin.Action != "removed" || fin.Committed || !fin.Pushed {
		t.Fatalf("fin = %+v", fin)
	}
	if remoteTip(remoteDir, wt.Branch) == "" {
		t.Fatal("remote should hold the branch")
	}
	if fileExists(wt.WorktreePath) {
		t.Fatal("worktree should be removed")
	}
}

func TestFinalizeRunWorktreeNoRemoteSkipsPush(t *testing.T) {
	repo := initRepo(t)
	wt, err := CreateWorktree(repo, "PUSH-4")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.WorktreePath, "wip.txt"), "local-only work\n")

	fin := FinalizeRunWorktree(FinalizeWorktreeOptions{
		RepoPath: repo, WorktreePath: wt.WorktreePath, TicketID: "PUSH-4", Mode: ModeRemove,
	})
	if fin.Action != "removed" || !fin.Committed || fin.Pushed || fin.Error != "" {
		t.Fatalf("fin = %+v", fin)
	}
	if fileExists(wt.WorktreePath) {
		t.Fatal("worktree should be removed")
	}
}
