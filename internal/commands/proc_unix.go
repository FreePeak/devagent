//go:build unix

package commands

import "syscall"

// processAlive mirrors the bash `kill -0 <pid>` liveness probe: true when
// the pid exists or the signal was refused for permission reasons, false
// only when the process is gone (ESRCH). Same contract as the loopdriver
// and scout copies (each package keeps its own — no shared plumbing).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
