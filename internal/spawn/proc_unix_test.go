//go:build unix

package spawn

import (
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// The early-completion kill must reap the whole tree, not just the direct
// child: an orphaned grandchild holding the stdout pipe is exactly the
// wedge the primitive exists to prevent (issue #308, from #273's finding).
func TestRunCliUntilKillsGrandchild(t *testing.T) {
	var grandPID int
	r := RunCliUntil("sh", []string{"-c", `sleep 60 & echo GC=$!; sleep 60`}, Options{TimeoutMs: 20_000},
		func(stdout string) bool {
			if i := strings.Index(stdout, "GC="); i >= 0 {
				if pid, err := strconv.Atoi(strings.TrimSpace(stdout[i+3:])); err == nil {
					grandPID = pid
				}
			}
			return strings.Contains(stdout, "GC=")
		})
	if r.ExitCode != -1 || r.TimedOut {
		t.Fatalf("early completion should be exit -1 not timedOut: %+v", r)
	}
	if grandPID <= 0 {
		t.Fatalf("never captured the grandchild pid from stdout: %q", r.Stdout)
	}
	deadline := time.Now().Add(5 * time.Second)
	for pidAlive(grandPID) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the tree kill", grandPID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// grandPIDFromStdout reads the "GC=<pid>" marker the fixture children below
// echo before they fork off into a long sleep.
func grandPIDFromStdout(t *testing.T, stdout string) int {
	t.Helper()
	i := strings.Index(stdout, "GC=")
	if i < 0 {
		t.Fatalf("no grandchild pid in stdout: %q", stdout)
	}
	pid, err := strconv.Atoi(strings.Fields(stdout[i+3:])[0])
	if err != nil {
		t.Fatalf("bad grandchild pid in %q: %v", stdout, err)
	}
	return pid
}

// waitPIDGone fails when pid is still alive after the bound.
func waitPIDGone(t *testing.T, pid int, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for pidAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: pid %d still alive", why, pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// RunCli's hard-timeout kill must reap the whole tree, not just the direct
// child (issue #308's retarget): the grandchild holds the inherited stdout
// write-end, so waiting it out is the #273 loop pin, not a timeout. The
// fixture sleeps far past the wall — pre-retarget the call returned only
// when that sleep ended.
func TestRunCliTimeoutKillsGrandchild(t *testing.T) {
	start := time.Now()
	r := RunCli("sh", []string{"-c", `sleep 45 & echo GC=$!; sleep 45`}, Options{TimeoutMs: 500})
	elapsed := time.Since(start)
	if !r.TimedOut || r.ExitCode != -1 {
		t.Fatalf("want timeout exit -1, got %+v", r)
	}
	grand := grandPIDFromStdout(t, r.Stdout)
	if elapsed > 15*time.Second {
		t.Fatalf("timeout took %s: an orphaned grandchild pinned the capture (issue #273)", elapsed)
	}
	waitPIDGone(t, grand, "RunCli timeout kill")
}
