package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #245: a dirty NON-PRD tracked file used to slip past the
// WorkSelectionDocs-only dirty gate, so the linear-history path proceeded
// into `git merge --ff-only`, git aborted ("Your local changes ... would be
// overwritten by merge"), and the sync returned a plain OK:false — CLI rc 1
// — which the loop driver classified as provider-degraded (fails++ and the
// degradation streak) instead of an operator class. The fix: a whole-tree
// dirty pre-check (behind → refuse OK:false/Dirty:true → exit 2; diverged →
// exit 3), plus defense-in-depth stderr classification on the ff failure
// path. The CLI exit mapping (Dirty→2, Diverged→3, checked Diverged-first)
// lives in internal/cli/actions_git.go; RepoSyncResult flags are the
// contract at this layer.

// commitAndPushTrackedFile commits a non-doc tracked file into the fixture
// repo and pushes it, so a later local edit to that file is tracked dirt.
func commitAndPushTrackedFile(t *testing.T, repo, origin, rel string) {
	t.Helper()
	writeFile(t, filepath.Join(repo, rel), "tracked v1\n")
	runGit(t, repo, "add", rel)
	runGit(t, repo, "commit", "-m", "add "+rel)
	runGit(t, repo, "push", "-q", origin, "main")
	// The seed clone (used by pushPrdUpdate/commitUpstreamFile) must catch
	// up or its next push is a non-fast-forward rejection.
	seed := filepath.Join(origin, "..", "seed")
	runGit(t, seed, "pull", "-q", origin, "main")
}

// commitUpstreamFile pushes a change to an arbitrary path from the seed
// clone, so origin can move a file the local copy has dirtied.
func commitUpstreamFile(t *testing.T, origin, rel, content, msg string) {
	t.Helper()
	seed := filepath.Join(origin, "..", "seed")
	writeFile(t, filepath.Join(seed, rel), content)
	runGit(t, seed, "add", rel)
	runGit(t, seed, "commit", "-m", msg)
	runGit(t, seed, "push", "-q", origin, "main")
}

// TestSyncWorkSelectionDocsRefusesDirtyNonDocFileBehind: the exact #245
// symptom fixture — dirty non-PRD tracked file + linear history behind
// origin must refuse with Dirty:true (CLI exit 2, operator-degraded), name
// the blocking file, and leave the residue and HEAD untouched.
func TestSyncWorkSelectionDocsRefusesDirtyNonDocFileBehind(t *testing.T) {
	repo, origin := initRepoWithOrigin(t)
	commitAndPushTrackedFile(t, repo, origin, "internal/loopdriver/run_test.go")
	pushPrdUpdate(t, origin, "v2") // repo is now strictly behind
	writeFile(t, filepath.Join(repo, "internal/loopdriver/run_test.go"), "sweep-in residue\n")

	r := SyncWorkSelectionDocs(repo, nil)
	if r.OK || !r.Dirty || r.Diverged {
		t.Fatalf("r = %+v", r)
	}
	if !strings.Contains(r.Detail, "refusing sync") || !strings.Contains(r.Detail, "internal/loopdriver/run_test.go") {
		t.Fatalf("detail does not refuse/names no blocking file: %q", r.Detail)
	}
	data, _ := os.ReadFile(filepath.Join(repo, "internal/loopdriver/run_test.go"))
	if string(data) != "sweep-in residue\n" {
		t.Fatalf("residue clobbered: %q", data)
	}
	if revParse(t, repo, "HEAD") == revParse(t, origin, "main") {
		t.Fatal("sync must not move HEAD past a dirty-tree refusal")
	}
}

// TestSyncWorkSelectionDocsDivergedDirtyNonDocFileRefuses: diverged + dirty
// non-doc tracked file takes the existing diverged+dirty refusal shape
// (CLI exit 3) instead of attempting the autostash rebase.
func TestSyncWorkSelectionDocsDivergedDirtyNonDocFileRefuses(t *testing.T) {
	repo, origin := initRepoWithOrigin(t)
	commitAndPushTrackedFile(t, repo, origin, "internal/pipeline/consume_test.go")
	// Local-only commit -> histories diverge once v2 lands on origin.
	writeFile(t, filepath.Join(repo, "scratch", "x.txt"), "local")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "local diverge")
	pushPrdUpdate(t, origin, "v2")
	writeFile(t, filepath.Join(repo, "internal/pipeline/consume_test.go"), "sweep-in residue\n")

	r := SyncWorkSelectionDocs(repo, nil)
	if r.OK || !r.Diverged || !r.Dirty {
		t.Fatalf("r = %+v", r)
	}
	if !strings.Contains(r.Detail, "reconcile by hand") {
		t.Fatalf("detail = %q", r.Detail)
	}
	if fileExists(filepath.Join(repo, ".git", "rebase-merge")) || fileExists(filepath.Join(repo, ".git", "rebase-apply")) {
		t.Fatal("rebase started despite dirty-tree refusal")
	}
}

// TestMergeAbortedOnLocalChanges pins the defense-in-depth classifier
// against the real git ff-abort stderr (unit-level; the classifier's
// end-to-end path is exercised by TestSyncWorkSelectionDocsStateDirRace).
func TestMergeAbortedOnLocalChanges(t *testing.T) {
	const ffAbort = "error: Your local changes to the following files would be overwritten by merge:\n\tinternal/loopdriver/run_test.go\nPlease commit your changes or stash them before you merge.\nAborting"
	if !mergeAbortedOnLocalChanges(ffAbort) {
		t.Fatal("real git ff-abort stderr not classified as dirty")
	}
	for _, other := range []string{
		"",
		"fatal: unable to access 'https://github.com/': Could not resolve host: github.com",
		"fatal: refusing to merge unrelated histories",
		"error: cannot lock ref 'refs/heads/main'",
	} {
		if mergeAbortedOnLocalChanges(other) {
			t.Fatalf("misclassified non-dirty failure: %q", other)
		}
	}
}

// TestSyncWorkSelectionDocsStateDirRace drives the defense-in-depth path
// end to end: a tracked file inside the excluded .selfbuild/ state dir is
// dirty locally while origin moves that same file, so the pre-check passes
// (state dirs are excluded) but the ff merge aborts on local changes — the
// classifier must still return Dirty:true (exit 2), never plain OK:false.
func TestSyncWorkSelectionDocsStateDirRace(t *testing.T) {
	repo, origin := initRepoWithOrigin(t)
	commitAndPushTrackedFile(t, repo, origin, ".selfbuild/ledger.json")
	commitUpstreamFile(t, origin, ".selfbuild/ledger.json", "origin v2\n", "origin moves state file")
	writeFile(t, filepath.Join(repo, ".selfbuild/ledger.json"), "local residue\n")

	r := SyncWorkSelectionDocs(repo, nil)
	if r.OK || !r.Dirty || r.Diverged {
		t.Fatalf("r = %+v", r)
	}
	if !strings.Contains(r.Detail, "commit or stash first") {
		t.Fatalf("detail = %q", r.Detail)
	}
	data, _ := os.ReadFile(filepath.Join(repo, ".selfbuild/ledger.json"))
	if string(data) != "local residue\n" {
		t.Fatalf("residue clobbered: %q", data)
	}
}

// TestSyncWorkSelectionDocsStateDirDirtDoesNotBlock: dirty state-dir files
// and untracked residue must NOT block a normal behind sync (existing
// semantics preserved by the whole-tree gate's filters).
func TestSyncWorkSelectionDocsStateDirDirtDoesNotBlock(t *testing.T) {
	repo, origin := initRepoWithOrigin(t)
	commitAndPushTrackedFile(t, repo, origin, ".devagent/loop-state.json")
	pushPrdUpdate(t, origin, "v2") // behind
	writeFile(t, filepath.Join(repo, ".devagent/loop-state.json"), "dirty state\n")
	writeFile(t, filepath.Join(repo, "goal.tmp"), "untracked scratch\n")

	r := SyncWorkSelectionDocs(repo, nil)
	if !r.OK || r.Dirty || r.Diverged {
		t.Fatalf("r = %+v", r)
	}
	if got, want := revParse(t, repo, "HEAD:docs/PRD.md"), revParse(t, origin, "main:docs/PRD.md"); got != want {
		t.Fatalf("PRD blob %q != origin %q", got, want)
	}
	// The local state edit and the untracked scratch survive the sync.
	data, _ := os.ReadFile(filepath.Join(repo, ".devagent/loop-state.json"))
	if string(data) != "dirty state\n" {
		t.Fatalf("state clobbered: %q", data)
	}
	if !fileExists(filepath.Join(repo, "goal.tmp")) {
		t.Fatal("untracked scratch removed")
	}
}

// TestDocTreeDirtyFilesPathspecFilter pins the helper contract directly:
// the git-level pathspec filter drops state-dir paths, -uno drops untracked
// residue, and plain tracked dirt survives.
func TestDocTreeDirtyFilesPathspecFilter(t *testing.T) {
	repo, _ := initRepoWithOrigin(t)
	writeFile(t, filepath.Join(repo, "internal/run_test.go"), "tracked v1\n")
	writeFile(t, filepath.Join(repo, ".devagent/x.json"), "{}\n")
	writeFile(t, filepath.Join(repo, ".selfbuild/y.json"), "{}\n")
	runGit(t, repo, "add", "internal/run_test.go", ".devagent/x.json", ".selfbuild/y.json")
	runGit(t, repo, "commit", "-m", "state files")
	writeFile(t, filepath.Join(repo, "internal/run_test.go"), "tracked v2\n")
	writeFile(t, filepath.Join(repo, ".devagent/x.json"), "{\"dirty\":true}\n")
	writeFile(t, filepath.Join(repo, ".selfbuild/y.json"), "{\"dirty\":true}\n")
	writeFile(t, filepath.Join(repo, "untracked.tmp"), "?\n")

	out, err := docTreeDirtyFiles(repo, 10_000)
	if err != nil {
		t.Fatalf("docTreeDirtyFiles: %v", err)
	}
	got := nonEmptyLines(strings.TrimSpace(out))
	if len(got) != 1 || !strings.HasSuffix(got[0], "internal/run_test.go") {
		t.Fatalf("docTreeDirtyFiles = %q, want only internal/run_test.go", got)
	}
}
