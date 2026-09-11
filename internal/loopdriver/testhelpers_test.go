package loopdriver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initFixtureRepo creates a hermetic git repo (no network, local user
// config) under t.TempDir and returns its path.
func initFixtureRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("config", "commit.gpgsign", "false")
	return repo
}

// addBareOrigin creates a local bare repo and wires it as `origin` on the
// fixture repo (fetch/push over the filesystem — fully hermetic).
func addBareOrigin(t *testing.T, repo string) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "origin.git")
	cmd := exec.Command("git", "init", "--bare", "-q", bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	r := exec.Command("git", "-C", repo, "remote", "add", "origin", bare)
	if out, err := r.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
	return bare
}

// writeRepoFile writes a file inside the fixture repo.
func writeRepoFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	path := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeBinDir builds a PATH dir with executable shell scripts. Each entry
// maps <name> → <body> (a shell script).
func fakeBinDir(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// loopConfigFor builds a hermetic LoopConfig over the fixture repo.
func loopConfigFor(t *testing.T, repo string, mutate func(*LoopConfig)) LoopConfig {
	t.Helper()
	cfg := LoopConfig{
		Repo:             repo,
		DevagentBin:      "devagent-fake",
		GhBin:            "gh-fake",
		DryRun:           true,
		NoSyncDocs:       true,
		PushMode:         "pr",
		MaxIterations:    2,
		CleanupDelaySecs: 1800,
		// Hermetic dispatch bins: the default omp router argv would leak
		// the real omp CLI from PATH on this machine.
		ResearchBin: "omp-fake",
		POBin:       "omp-fake",
		// Every hermetic RunLoop test that reaches a terminal verdict arms
		// the #286 exit watchdog; a real os.Exit would kill the test
		// binary. Watchdog-specific tests override it via mutate.
		Exit: func(int) {},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg.WithDefaults()
}

// assertFileContains fails the test when the file does not exist or misses
// want.
func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s does not contain %q:\n%s", path, want, data)
	}
}
