//go:build unix

// Process-group kill for the stale-worker reaper (unix): SIGTERM the group
// first (negative pid), then direct; after an 800ms grace, SIGKILL the
// group. Windows gets a documented degradation in reaper_windows.go.

package pipeline

import (
	"syscall"
	"time"
)

// killStaleProcessTree mirrors the TS killStaleProcessTree (the TS `reason`
// parameter is dead in the original and dropped here). SIGTERM the process
// group first (negative pid), then direct; after an 800ms grace, SIGKILL
// the group.
func killStaleProcessTree(pid int) bool {
	if pid <= 1 {
		return false
	}
	if !isWorkerPid(pid) {
		return false
	}
	// Try process group first (negative pid), then direct.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			return false
		}
	}
	// Brief grace then SIGKILL.
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return true
}
