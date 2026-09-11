//go:build unix

package ledger

import "syscall"

// processAlive mirrors loopdriver's liveness check: true only when the
// signal is deliverable. Every non-nil error reads as dead — same verdict
// surface as scout's and loopdriver's GOOS splits.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
