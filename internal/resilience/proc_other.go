//go:build !unix

package resilience

import "os/exec"

// setProbeProcessGroup has no portable process-group primitive off unix;
// the documented degradation (same as loopdriver's dispatch sweep) is the
// direct-child kill below.
func setProbeProcessGroup(cmd *exec.Cmd) {}

// killProbeProcessGroup kills the direct child only.
func killProbeProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
