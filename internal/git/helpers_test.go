package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGit runs a git command in dir, failing the test on error, and returns
// trimmed stdout (fixture helper, mirrors the TS tests' execFileSync usage).
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %s: %v\nstderr: %s", strings.Join(args, " "), dir, err, stderrOf(err))
	}
	return strings.TrimSpace(string(out))
}

// runGitFail runs a git command expected to fail (e.g. merge-base
// --is-ancestor when the ancestry does not hold).
func runGitFail(t *testing.T, dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if _, err := cmd.Output(); err == nil {
		t.Fatalf("git %s in %s unexpectedly succeeded", strings.Join(args, " "), dir)
	}
}

func stderrOf(err error) string {
	if ee, ok := err.(*exec.ExitError); ok {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}

// writeFile creates a file with its parent directories.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// initRepo creates a temp repo with one commit on main (mirrors the TS
// fixtures; -b main pins the branch the clean-main-worktree guard asserts).
func initRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "test")
	writeFile(t, filepath.Join(repo, "f.txt"), "x\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "init")
	return repo
}

// initRepoWithRemote wires a repo to a local bare `origin` over the file
// transport: exercises the real push path (snapshot-then-push) without
// touching the network (mirrors initRepoWithRemote).
func initRepoWithRemote(t *testing.T) (repo, remoteDir string) {
	t.Helper()
	base := t.TempDir()
	remoteDir = filepath.Join(base, "origin.git")
	runGit(t, base, "init", "--bare", remoteDir)
	repo = filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "test")
	writeFile(t, filepath.Join(repo, "f.txt"), "x\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "init")
	runGit(t, repo, "remote", "add", "origin", remoteDir)
	return repo, remoteDir
}

// remoteTip is the tip of a branch in the bare remote (” when the remote
// never saw it).
func remoteTip(remoteDir, branch string) string {
	out, err := exec.Command("git", "--git-dir", remoteDir, "rev-parse", "--verify", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// initFixture builds a temp bare repo as the remote plus a work repo with one
// initial commit on main (mirrors initFixture in test/state-branch.test.ts).
func initFixture(t *testing.T) (repo, bare string) {
	t.Helper()
	base := t.TempDir()
	repo = filepath.Join(base, "repo")
	bare = filepath.Join(base, "remote.git")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, base, "init", "--bare", "-b", "main", bare)
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "test")
	runGit(t, repo, "remote", "add", "origin", bare)
	writeFile(t, filepath.Join(repo, "f.txt"), "x\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "init")
	return repo, bare
}
