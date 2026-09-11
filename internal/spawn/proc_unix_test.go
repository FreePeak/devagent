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
