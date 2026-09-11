//go:build unix

package spawn

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setOwnProcessGroup puts the child into its own process group (pgid == its
// pid) so RunCliUntil's kill reaches grandchildren too: a CLI that fans out
// into its own children survives CommandContext's direct-child kill and keeps
// the output pipes open (issue #273's loop pin, generalized to the
// early-completion kill of issue #308).
func setOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// treeKillRounds bounds the group sweep: each round signals the members alive
// at that instant, so a survivor forked just before its parent's death is
// caught by the next round (5 ms apart, 10 rounds — enough for ~9 generations
// of fan-out).
const (
	treeKillRounds    = 10
	treeKillRoundStep = 5 * time.Millisecond
)

// killProcessTree does BOTH the group sweep and the direct Process.Kill: the
// sweep reaps unix grandchildren, the direct kill is the floor that must
// never be lost on any platform.
//
// The sweep REPEATS, and the direct kill goes FIRST. kill(-pgid) signals only
// the members alive at that instant, so a one-shot sweep can miss a child
// forked microseconds later by a parent that is already marked dead: measured,
// a marker hit at t=214ms with kill(-pgid) returning nil, and the orphan
// (`sleep`, pgid = the killed child's pid) survived holding the inherited
// stdout write-end. RunCliUntil's reader then never saw EOF, so it never
// reached cmd.Wait and the caller sat in its final wait until the context
// deadline — a verify-and-merge iteration reporting the right verdict 20 s
// late. Killing the leader first removes whoever does the forking; each later
// round catches the previous round's survivors. ESRCH means the group is gone.
func killProcessTree(p *os.Process) {
	_ = p.Kill()
	for i := 0; i < treeKillRounds; i++ {
		if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
			return // ESRCH: no members left (or the group never existed)
		}
		time.Sleep(treeKillRoundStep)
	}
}
