//go:build !unix

package loopdriver

import "os/exec"

// processAlive has no portable signal probe off unix; report alive and let
// the caller's stale-lock retry path handle it.
func processAlive(pid int) bool { return pid > 0 }

// setOwnProcessGroup has no portable process-group primitive off unix;
// CommandContext's direct-child kill is the documented degradation (issue
// #273's group sweep is unix-only, like the pipeline reaper).
func setOwnProcessGroup(cmd *exec.Cmd) {}

// killDispatchTree has no portable signal probe off unix; nothing to sweep.
func killDispatchTree(pid int) {}
