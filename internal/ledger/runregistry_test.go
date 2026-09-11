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

// TestReleaseKeepsReplacedLock pins issue #316's Release contract: a lock
// holder whose file was replaced by a later acquisition (stale-break winner)
// must not delete the winner's live lock on its deferred Release — and its
// own payload must still be removable. Verified live 2026-09-11 both ways:
// older-exit deleted a newer run's lock, and a stale-break stole a live
// older lock.
func TestReleaseKeepsReplacedLock(t *testing.T) {
	home := t.TempDir()
	base := NowFunc()
	l1 := TryAcquireRun(home, "ticket/one", 0)
	if l1 == nil {
		t.Fatal("fresh acquire must succeed")
	}
	// A later holder overwrites the file: different startedAt, so the bytes
	// no longer belong to l1 even though the pid matches (same test process).
	later := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"startedAt":` + strconv.FormatInt(base+1, 10) + `}`
	if err := os.WriteFile(l1.Path, []byte(later), 0o644); err != nil {
		t.Fatal(err)
	}
	l1.Release()
	raw, err := os.ReadFile(l1.Path)
	if err != nil || string(raw) != later {
		t.Fatalf("release deleted a later holder's lock (err=%v, raw=%s)", err, raw)
	}
	// With the file back to l1's own payload a release must unlink. l1 has
	// spent its idempotent release above, so drive the same acquisition
	// through a fresh lock value (same pid+startedAt the file carries).
	own := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"startedAt":` + strconv.FormatInt(base, 10) + `}`
	if err := os.WriteFile(l1.Path, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	l1b := &RunLock{TicketID: l1.TicketID, Path: l1.Path, pid: int64(os.Getpid()), startedAt: base}
	l1b.Release()
	if _, err := os.Stat(l1.Path); !os.IsNotExist(err) {
		t.Fatal("own lock must be removed by release")
	}
}
