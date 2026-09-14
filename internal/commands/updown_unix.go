//go:build unix

package commands

import (
	"fmt"
	"os/exec"
	"syscall"
)

// detachAttrs puts a child in its own session and process group: it survives
// `devagent up` exiting, and — because the driver becomes the group leader —
// one group signal from `devagent down` reaps the driver together with every
// worker it dispatched. The daemon detaches the same way (detach_unix.go).
func detachAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// terminateTree signals the child's whole process group, falling back to the
// bare pid when the child was not a group leader — the shape of a driver
// started by `make loop-start` or a bare `devagent loop`, whose lock record
// `down` can still find.
func terminateTree(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err == nil {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	return nil
}
