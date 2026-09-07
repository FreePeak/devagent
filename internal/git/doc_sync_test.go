package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Ports test/doc-sync.test.ts (vitest): real local git repo with origin = a
// second bare clone so fetch/ff/diverged logic is proven end to end, not
// mocked. (The TS suite also has a CLI smoke section for `devagent sync-docs`
// — out of scope here; the wiring lives in internal/cli.)

// initRepoWithOrigin builds the doc-sync fixture: a seed clone, a bare
// origin, and the repo under test cloned from it.
func initRepoWithOrigin(t *testing.T) (repo, origin string) {
	t.Helper()
	base := t.TempDir()
	seed := filepath.Join(base, "seed")
	origin = filepath.Join(base, "origin.git")
	repo = filepath.Join(base, "repo")
	runGit(t, base, "init", "--bare", origin)
	runGit(t, base, "init", seed)
	runGit(t, seed, "config", "user.email", "t@t")
	runGit(t, seed, "config", "user.name", "t")
	writeFile(t, filepath.Join(seed, "docs", "PRD.md"), "# PRD v1\n")
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-m", "v1")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-q", origin, "main")
	// A bare repo's HEAD is unborn until pointed at main; cloning before this
	// fix yields an empty checkout and "unknown revision HEAD".
	runGit(t, origin, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, base, "clone", "-q", origin, repo)
	runGit(t, repo, "config", "user.name", "t")
	// Some environments carry no global git identity; the diverged scenarios
	// commit inside the clone, so pin the email locally too.
	runGit(t, repo, "config", "user.email", "t@t")
	return repo, origin
}

// pushPrdUpdate simulates an operator pushing a manual PRD edit from another
// machine (the seed clone).
func pushPrdUpdate(t *testing.T, origin, version string) {
	t.Helper()
	seed := filepath.Join(origin, "..", "seed")
	writeFile(t, filepath.Join(seed, "docs", "PRD.md"), "# PRD "+version+"\n")
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-m", version)
	runGit(t, seed, "push", "-q", origin, "main")
}

func revParse(t *testing.T, dir, rev string) string {
	t.Helper()
	return runGit(t, dir, "rev-parse", rev)
}

func TestSyncWorkSelectionDocsFastForwardsStaleRepo(t *testing.T) {
	repo, origin := initRepoWithOrigin(t)

	before := revParse(t, repo, "HEAD")
	up1 := SyncWorkSelectionDocs(repo, nil)
	if !up1.OK || !up1.AlreadyUpToDate {
		t.Fatalf("up1 = %+v", up1)
	}

	// Operator pushes a manual PRD edit from "another machine" (the seed clone).
	pushPrdUpdate(t, origin, "v2")

	up2 := SyncWorkSelectionDocs(repo, nil)
	if !up2.OK || up2.AlreadyUpToDate {
		t.Fatalf("up2 = %+v", up2)
	}

	if revParse(t, repo, "HEAD") == before {
		t.Fatal("HEAD did not move")
	}
	if got, want := revParse(t, repo, "HEAD:docs/PRD.md"), revParse(t, origin, "main:docs/PRD.md"); got != want {
		t.Fatalf("PRD blob %q != origin %q", got, want)
	}
}

func TestSyncWorkSelectionDocsRefusesLocallyModifiedPRD(t *testing.T) {
	repo, origin := initRepoWithOrigin(t)
	pushPrdUpdate(t, origin, "v2")
	writeFile(t, filepath.Join(repo, "docs", "PRD.md"), "# operator mid-edit\n")

	r := SyncWorkSelectionDocs(repo, nil)
	if r.OK {
		t.Fatalf("r = %+v", r)
	}
	if !strings.Contains(r.Detail, "refusing sync") {
		t.Fatalf("detail = %q", r.Detail)
	}
	data, _ := os.ReadFile(filepath.Join(repo, "docs", "PRD.md"))
	if !strings.Contains(string(data), "operator mid-edit") {
		t.Fatalf("PRD clobbered: %q", data)
	}
}

func TestSyncWorkSelectionDocsDivergedCleanRebases(t *testing.T) {
	repo, origin := initRepoWithOrigin(t)
	// Local commit origin will never have -> histories diverge once v2 lands.
	writeFile(t, filepath.Join(repo, "scratch", "x.txt"), "local")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "local diverge")
	pushPrdUpdate(t, origin, "v2")

	r := SyncWorkSelectionDocs(repo, nil)
	if !r.OK || !r.Diverged {
		t.Fatalf("r = %+v", r)
	}
	// The local commit survived the rebase and the PRD matches origin.
	if log := runGit(t, repo, "log", "--format=%s", "-2"); !strings.Contains(log, "local diverge") {
		t.Fatalf("log = %q", log)
	}
	if got, want := revParse(t, repo, "HEAD:docs/PRD.md"), revParse(t, origin, "main:docs/PRD.md"); got != want {
		t.Fatalf("PRD blob %q != origin %q", got, want)
	}
}

func TestSyncWorkSelectionDocsDivergedConflictAbortsCleanly(t *testing.T) {
	repo, origin := initRepoWithOrigin(t)
	// Both sides rewrite docs/PRD.md -> rebase must hit a conflict.
	writeFile(t, filepath.Join(repo, "docs", "PRD.md"), "# local divergent PRD\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "conflicting local PRD")
	pushPrdUpdate(t, origin, "v2 conflicting")

	r := SyncWorkSelectionDocs(repo, nil)
	if r.OK || !r.Diverged || r.Dirty {
		t.Fatalf("r = %+v", r)
	}
	if !strings.Contains(r.Detail, "aborted cleanly") {
		t.Fatalf("detail = %q", r.Detail)
	}
	// Clean abort: no rebase left in progress, HEAD still the local commit.
	if fileExists(filepath.Join(repo, ".git", "rebase-merge")) || fileExists(filepath.Join(repo, ".git", "rebase-apply")) {
		t.Fatal("rebase state left behind")
	}
	if got := runGit(t, repo, "log", "--format=%s", "-1"); got != "conflicting local PRD" {
		t.Fatalf("HEAD subject = %q", got)
	}
	// The PRD still reflects the local commit, not origin's.
	data, _ := os.ReadFile(filepath.Join(repo, "docs", "PRD.md"))
	if !strings.Contains(string(data), "local divergent PRD") {
		t.Fatalf("PRD = %q", data)
	}
}

func TestSyncWorkSelectionDocsDivergedDirtyRefuses(t *testing.T) {
	repo, origin := initRepoWithOrigin(t)
	// Diverge via a committed local change, then dirty the PRD on top.
	writeFile(t, filepath.Join(repo, "scratch", "x.txt"), "local")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "local diverge")
	pushPrdUpdate(t, origin, "v2")
	writeFile(t, filepath.Join(repo, "docs", "PRD.md"), "# operator mid-edit on diverged tree\n")

	r := SyncWorkSelectionDocs(repo, nil)
	if r.OK || !r.Diverged || !r.Dirty {
		t.Fatalf("r = %+v", r)
	}
	if !strings.Contains(r.Detail, "reconcile by hand") {
		t.Fatalf("detail = %q", r.Detail)
	}
	// The operator's uncommitted edit is untouched and no rebase started.
	data, _ := os.ReadFile(filepath.Join(repo, "docs", "PRD.md"))
	if !strings.Contains(string(data), "operator mid-edit on diverged tree") {
		t.Fatalf("PRD = %q", data)
	}
	if fileExists(filepath.Join(repo, ".git", "rebase-merge")) {
		t.Fatal("rebase started")
	}
}

func TestSyncWorkSelectionDocsUnreachableOriginIsFailureNotThrow(t *testing.T) {
	repo := t.TempDir() + "/repo"
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	r := SyncWorkSelectionDocs(repo, nil)
	if r.OK {
		t.Fatalf("r = %+v", r)
	}
	if !strings.Contains(r.Detail, "git fetch failed") {
		t.Fatalf("detail = %q", r.Detail)
	}
}

func TestPrdStat(t *testing.T) {
	repo, _ := initRepoWithOrigin(t)
	if PrdStat(repo) == nil {
		t.Fatal("PRD.md exists; stat must resolve")
	}
	empty := t.TempDir()
	if PrdStat(empty) != nil {
		t.Fatal("missing PRD.md must return nil")
	}
}
