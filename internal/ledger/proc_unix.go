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

// ProcessAlive is the exported surface of the check: true only when the
// signal is deliverable to pid. Shared by the run-registry lock break and
// the daemon's runs.active counter so "fresh" and "alive" stop being
// conflated (a >1h task was reported inactive while its lock holder lived).
func ProcessAlive(pid int) bool { return processAlive(pid) }
