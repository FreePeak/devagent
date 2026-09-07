//go:build unix

package herdr

import "syscall"

// detachProcAttr mirrors Node's spawn({ detached: true }): the pane server
// daemon becomes a session leader and outlives the devagent process.
func detachProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
