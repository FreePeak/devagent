package ledger

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// deadPid returns the pid of a process that has already exited — a real,
// verifiably dead holder for lock-break tests (never a guessed number).
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn true: %v", err)
	}
	return cmd.ProcessState.Pid()
}

// TestTryAcquireRunBreaksDeadHolderLock pins the #316-class live failure
// (2026-09-11): a breaker-killed incarnation left TASK.lock holding a dead
// pid inside the 1h TTL, and every devagent task dispatch died with
// "Run for TASK already active" — one orphaned lock bricked the selfbuild
// loop. A dead holder must break the lock immediately, TTL notwithstanding.
func TestTryAcquireRunBreaksDeadHolderLock(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "locks", "TASK.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	pid := deadPid(t)
	fresh := NowFunc() // well inside the 1h TTL
	payload := `{"pid":` + strconv.Itoa(pid) + `,"startedAt":` + strconv.FormatInt(fresh, 10) + `}`
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	lock := TryAcquireRun(home, "TASK", 0)
	if lock == nil {
		t.Fatalf("lock with DEAD holder pid %d refused acquisition inside TTL — the live failure", pid)
	}
	lock.Release()
}

// TestTryAcquireRunRefusesFreshLiveHolder pins the kept rule: a fresh lock
// whose holder is THIS process (verifiably alive) must still be refused —
// liveness only breaks locks whose holder is gone.
func TestTryAcquireRunRefusesFreshLiveHolder(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "locks", "TASK.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := NowFunc()
	payload := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"startedAt":` + strconv.FormatInt(fresh, 10) + `}`
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	if lock := TryAcquireRun(home, "TASK", 0); lock != nil {
		t.Fatal("fresh lock with live holder must NOT be broken")
	}
}

// TestReleaseOnlyRemovesOwnLock pins the second half of the #316 live
// failure: run A's lock was broken as stale and re-acquired by run B, then
// A's `defer lock.Release()` unlinked B's LIVE lock (09:51:16), so
// countActiveRuns saw 0 while two tasks ran. Release must leave a lock whose
// payload is no longer this acquisition's.
func TestReleaseOnlyRemovesOwnLock(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "locks", "TASK-abc.lock")

	mine := TryAcquireRun(home, "TASK-abc", 0)
	if mine == nil {
		t.Fatal("fresh acquire must succeed")
	}

	// The latest-wins break + re-acquire another run would do: a different
	// holder identity in the same file.
	other := `{"pid":` + strconv.Itoa(deadPid(t)) + `,"startedAt":` + strconv.FormatInt(NowFunc(), 10) + `}`
	if err := os.WriteFile(path, []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}

	mine.Release()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("release deleted a lock it no longer owned: %v", err)
	}
	if string(data) != other {
		t.Fatalf("release rewrote the foreign lock: %q", data)
	}

	// Its own lock is still removed on release.
	own := TryAcquireRun(home, "TASK-abc", 0)
	if own == nil {
		t.Fatal("dead foreign holder must be re-acquirable")
	}
	own.Release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("own lock must be removed on release, stat err = %v", err)
	}
}
