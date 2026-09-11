//go:build unix

package spawn

import (
	"os"
	"os/exec"
	"syscall"
)

// setOwnProcessGroup puts the child into its own process group (pgid == its
// pid) so RunCliUntil's kill reaches grandchildren too: a CLI that fans out
// into its own children survives CommandContext's direct-child kill and keeps
// the output pipes open (issue #273's loop pin, generalized to the
// early-completion kill of issue #308).
func setOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree does BOTH the group sweep and the direct Process.Kill: the
// sweep reaps unix grandchildren, the direct kill is the floor that must
// never be lost on any platform.
func killProcessTree(p *os.Process) {
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	_ = p.Kill()
}
