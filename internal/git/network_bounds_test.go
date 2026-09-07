package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bounded-network op semantics (issue #195 acceptance): every state-branch
// network command carries the driver's bounded GIT_SSH_COMMAND
// (BatchMode + ConnectTimeout) plus a hard wall-clock timeout, so a dead
// network fails fast instead of hanging the loop; a failed push is absorbed
// by the caller and deferred to the next run.

// fakeSSHOnPath drops an `ssh` shim on PATH that records its argv and fails
// like an unreachable host would (exit 255), so a git command that honours
// GIT_SSH_COMMAND is observable without touching the network.
func fakeSSHOnPath(t *testing.T) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ssh-argv.txt")
	bin := filepath.Join(t.TempDir(), "sshbin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + marker + "\nexit 255\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func TestEnsureStateBranchNetworkOpsUseBoundedGitSSHCommand(t *testing.T) {
	repo, _ := initFixture(t)
	writeFile(t, repo+"/.devagent/lessons.md", "seed\n")
	// Force the ssh transport: git only honours GIT_SSH_COMMAND for it.
	runGit(t, repo, "remote", "set-url", "origin", "git@invalid.invalid:devagent/none.git")
	marker := fakeSSHOnPath(t)

	if _, err := EnsureStateBranch(repo, nil); err == nil {
		t.Fatal("ls-remote against a dead ssh host must fail")
	} else if !strings.Contains(err.Error(), "git ls-remote --heads") {
		t.Fatalf("error should name the failing command: %q", err.Error())
	}

	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal("ssh was never invoked: GIT_SSH_COMMAND did not reach the child")
	}
	line := strings.TrimSpace(string(raw))
	if !strings.Contains(line, "BatchMode=yes") || !strings.Contains(line, "ConnectTimeout=10") {
		t.Fatalf("bounded ssh options missing: %q", line)
	}
}

// A push that fails is reported as an error the caller absorbs ("[state] push
// deferred"); the local repo must be left completely untouched so the next
// run retries from the same state.
func TestEnsureStateBranchPushFailureIsAbsorbedAndDeferred(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only remote requires non-root")
	}
	repo, bare := initFixture(t)
	writeFile(t, repo+"/.devagent/lessons.md", "seed\n")

	headBefore := runGit(t, repo, "rev-parse", "HEAD")
	// ls-remote (read) still succeeds against a read-only bare repo; the push
	// (write) cannot land.
	if err := filepath.WalkDir(bare, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o555)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(bare, func(p string, d os.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})

	res, err := EnsureStateBranch(repo, nil)
	if err == nil {
		t.Fatalf("push to a read-only remote must fail, got %+v", res)
	}
	if !strings.Contains(err.Error(), "git push") {
		t.Fatalf("failure should be the push, not an earlier step: %q", err.Error())
	}
	// Deferred, not destroyed: nothing local moved, and the remote ref is
	// still absent, so the next run retries the same creation.
	if runGit(t, repo, "rev-parse", "HEAD") != headBefore {
		t.Fatal("HEAD moved on a failed state-branch push")
	}
	out, _, _ := run([]string{"ls-remote", "--heads", "origin", "selfbuild/state"}, repo, 30_000)
	if strings.TrimSpace(out) != "" {
		t.Fatalf("remote should not hold the branch: %q", out)
	}
}
