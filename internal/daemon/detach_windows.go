//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// setDetach applies the platform detach attributes to cmd. On Windows:
// CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS matches the TS
// `detached: true` spawn semantics (no console, survives daemon exit).
func setDetach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}
