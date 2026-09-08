package loopdriver

import (
	"os"
	"path/filepath"
	"testing"
)

// gateDriver builds a driver over the fixture repo with TestCmd overridden;
// an empty testCmd keeps the WithDefaults `npm test` fallback.
func gateDriver(t *testing.T, repo, testCmd string) *driver {
	t.Helper()
	cfg := LoopConfig{Repo: repo, TestCmd: testCmd}.WithDefaults()
	return &driver{cfg: cfg, stateDir: filepath.Join(cfg.Repo, ".selfbuild")}
}

// gateFake puts an executable logging shim on PATH: it appends its argv to
// $GATE_LOG and touches gate-ran-here in its cwd (cwd proof without
// resolving the /var → /private/var tempdir symlink).
func gateFake(t *testing.T, name, logPath string) {
	t.Helper()
	dir := fakeBinDir(t, map[string]string{
		name: `#!/bin/sh
echo "` + name + ` $*" >> "$GATE_LOG"
touch gate-ran-here
exit 0
`,
	})
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("GATE_LOG", logPath)
}

func TestRunRepoTestsDefaultIsNpmTest(t *testing.T) {
	repo := initFixtureRepo(t)
	logPath := filepath.Join(repo, "gate-calls.log")
	gateFake(t, "npm", logPath)
	d := gateDriver(t, repo, "")
	if rc := d.runRepoTests(); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	// Default gate is exactly `npm test` (no argv), run inside cfg.Repo.
	assertFileContains(t, logPath, "npm test\n")
	if _, err := os.Stat(filepath.Join(repo, "gate-ran-here")); err != nil {
		t.Fatalf("gate did not run in cfg.Repo: %v", err)
	}
}

func TestRunRepoTestsWordSplitsTestCmd(t *testing.T) {
	repo := initFixtureRepo(t)
	logPath := filepath.Join(repo, "gate-calls.log")
	gateFake(t, "gate-fake", logPath)
	d := gateDriver(t, repo, "gate-fake alpha beta")
	if rc := d.runRepoTests(); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	assertFileContains(t, logPath, "gate-fake alpha beta\n")
}

func TestRunRepoTestsRcPlumbing(t *testing.T) {
	repo := initFixtureRepo(t)
	if rc := gateDriver(t, repo, "true").runRepoTests(); rc != 0 {
		t.Fatalf("passing gate rc = %d, want 0", rc)
	}
	if rc := gateDriver(t, repo, "false").runRepoTests(); rc != 1 {
		t.Fatalf("failing gate rc = %d, want 1", rc)
	}
	// A missing command gates as a failure (the FR-GO-16 ENOENT case),
	// never a crash.
	if rc := gateDriver(t, repo, "no-such-gate-bin").runRepoTests(); rc != 1 {
		t.Fatalf("missing-command gate rc = %d, want 1", rc)
	}
}
