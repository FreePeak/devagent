package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// deadPid returns the pid of a process that has already exited — a real,
// verifiably dead holder (never a guessed number).
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn true: %v", err)
	}
	return cmd.ProcessState.Pid()
}

// TestCountActiveRunsCountsLiveHolderPastTTL pins the #287-family status lie
// (verified live 2026-09-11): an 87-minute task — 60m wall + infra retry —
// showed runs.active 0 the whole time because its lock outlived the 1h TTL
// while its holder was still editing files. A LIVE holder must count
// regardless of age.
func TestCountActiveRunsCountsLiveHolderPastTTL(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour).UnixMilli()
	lock := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"startedAt":` + strconv.FormatInt(old, 10) + `}`
	if err := os.WriteFile(filepath.Join(home, "locks", "TASK.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := countActiveRuns(home); got != 1 {
		t.Fatalf("runs.active = %d, want 1: a live holder must count regardless of TTL age", got)
	}
}

// TestCountActiveRunsIgnoresDeadHolderInsideTTL pins the other half: a lock
// whose holder is dead must NOT count even when it is TTL-fresh — the exact
// stale TASK.lock that bricked dispatches on 2026-09-11 (dead pid 6762) must
// never read as an active run.
func TestCountActiveRunsIgnoresDeadHolderInsideTTL(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := time.Now().UnixMilli()
	lock := `{"pid":` + strconv.Itoa(deadPid(t)) + `,"startedAt":` + strconv.FormatInt(fresh, 10) + `}`
	if err := os.WriteFile(filepath.Join(home, "locks", "TASK.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := countActiveRuns(home); got != 0 {
		t.Fatalf("runs.active = %d, want 0: a dead holder inside the TTL is not an active run", got)
	}
}
