//go:build unix

package loopdriver

import "syscall"

// processAlive mirrors the bash `kill -0 <pid>` liveness probe: true when
// the pid exists or the signal was refused for permission reasons, false
// only when the process is gone (ESRCH).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
