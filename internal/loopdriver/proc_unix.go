//go:build unix

package loopdriver

import (
	"os/exec"
	"syscall"
)

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

// setOwnProcessGroup puts a dispatch child into its own process group
// (pgid == its pid) so the taskDispatch wall can kill the whole dispatched
// tree: the devagent CLI fans out into worker CLIs, and CommandContext's
// kill reaches only the direct child (issue #273).
func setOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killDispatchTree SIGKILLs the dispatch child's surviving process group.
// Called after Wait returned with the wall expired: the direct child is
// already dead, the survivors are orphaned worker sessions pinning the
// loop. ESRCH (nothing left in the group) is fine — ignored.
func killDispatchTree(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
