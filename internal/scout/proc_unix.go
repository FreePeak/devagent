//go:build unix

package scout

import "syscall"

// processAlive mirrors process.kill(pid, 0): true only when the signal is
// deliverable. TS treats EPERM as dead (its catch returns false), so the
// Go port returns false for every non-nil error — same verdict surface.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
