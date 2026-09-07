//go:build unix

package daemon

import (
	"os/exec"
	"syscall"
)

// setDetach applies the platform detach attributes to cmd. On Unix: setsid
// detaches the child into its own session and process group, matching the
// TS `detached: true` spawn (the child survives daemon exit).
func setDetach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
