//go:build unix

package resilience

import (
	"os/exec"
	"syscall"
)

// setProbeProcessGroup puts the probe child into its own process group
// (pgid == its pid) so the probe can kill the whole tree when the answer
// streams or the wall cap fires: worker CLIs fan out into children, and a
// direct-child kill leaves them orphaned (issue #273's finding, reused
// here on the probe path).
func setProbeProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProbeProcessGroup SIGKILLs the probe child's process group. ESRCH
// (group already gone) is fine — ignored.
func killProbeProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
