//go:build !unix

package commands

import (
	"fmt"
	"os"
	"os/exec"
)

// detachAttrs has no setsid equivalent off unix; the child inherits the
// parent's group and is reaped by pid alone (loopdriver's proc_other.go
// carries the same documented degradation).
func detachAttrs(cmd *exec.Cmd) {}

// terminateTree stops the recorded pid. Off unix there is no process-group
// probe here, so a worker the driver spawned may outlive it; the sweep and
// the loop lock remain the backstops.
func terminateTree(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find %d: %w", pid, err)
	}
	return p.Kill()
}
