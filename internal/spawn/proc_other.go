//go:build !unix

package spawn

import (
	"os"
	"os/exec"
)

// SetOwnProcessGroup has no portable process-group primitive off unix; the
// direct Process.Kill below is the documented degradation (issue #308, like
// issue #273's group sweep in loopdriver).
func SetOwnProcessGroup(cmd *exec.Cmd) {}

// KillProcessTree falls back to the direct kill off unix: windows CI must
// never lose the CommandContext-equivalent floor.
func KillProcessTree(p *os.Process) {
	_ = p.Kill()
}
