package loopdriver

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// cleanupRow is one cleanup-pending.jsonl row. Loop is a JSON *string*
// because the bash printf quoted it — byte-compat requires the same shape.
type cleanupRow struct {
	Loop     string `json:"loop"`
	TS       int64  `json:"ts"`
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"`
}

func (d *driver) pendingPath() string { return d.stateDir + "/cleanup-pending.jsonl" }

// scheduleCleanup ports schedule_cleanup(): record an auto-pr leftover pair
// for deferred sweep after the CLEANUP_DELAY grace period. Crash-safe: rows
// survive driver restarts.
func (d *driver) scheduleCleanup(loopNum int) {
	row := cleanupRow{
		Loop:     strconv.Itoa(loopNum),
		TS:       d.cfg.Now().Unix(),
		Branch:   "devagent/TASK",
		Worktree: d.cfg.Repo + "/.devagent-worktrees/TASK",
	}
	data, err := json.Marshal(row)
	if err != nil {
		return
	}
	f, err := os.OpenFile(d.pendingPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(data, '\n'))
}

// gitQuiet runs a local git command in the repo; returns (stdout, ok) with
// ok = exit 0.
func (d *driver) gitQuiet(args ...string) (string, bool) {
	cmd := exec.Command("git", args...)
	cmd.Dir = d.cfg.Repo
	out, err := cmd.Output()
	return string(out), err == nil
}

// remoteBranchTip returns the remote tip sha for a branch via unbounded
// ls-remote (bash parity — the 2026-09-06 bounded-ssh fix covered only the
// state-sync call sites), or "" when the branch is absent.
func (d *driver) remoteBranchTip(branch string) string {
	out, ok := d.gitQuiet("ls-remote", "origin", "refs/heads/"+branch)
	if !ok {
		return ""
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// sweepCleanup ports sweep_cleanup(): drop entries whose branch is gone,
// delete worktree+branch once the tip is verified on origin (the PR
// actually exists), defer everything else. Rows the bash regexes could not
// parse (no extractable ts) are kept; the file is rewritten with only the
// kept rows, and deleted when nothing is kept.
func (d *driver) sweepCleanup(logF io.Writer) {
	data, err := os.ReadFile(d.pendingPath())
	if err != nil {
		return
	}
	now := d.cfg.Now().Unix()
	var keep []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var row cleanupRow
		_ = json.Unmarshal([]byte(line), &row)
		if row.TS == 0 || now-row.TS < int64(d.cfg.CleanupDelaySecs) {
			keep = append(keep, line)
			continue
		}
		if _, ok := d.gitQuiet("show-ref", "--verify", "--quiet", "refs/heads/"+row.Branch); !ok {
			_, _ = fmt.Fprintf(logF, "[cleanup] %s already gone\n", row.Branch)
			_, _ = d.gitQuiet("worktree", "remove", "--force", row.Worktree)
			continue
		}
		localTip, _ := d.gitQuiet("rev-parse", "refs/heads/"+row.Branch)
		remoteTip := d.remoteBranchTip(row.Branch)
		if remoteTip != "" && strings.TrimSpace(localTip) == remoteTip {
			if _, ok := d.gitQuiet("worktree", "remove", "--force", row.Worktree); ok {
				_, _ = d.gitQuiet("branch", "-D", row.Branch)
				_, _ = fmt.Fprintf(logF, "[cleanup] removed %s + %s\n", row.Branch, row.Worktree)
			}
			// The row is dropped even if the worktree removal failed (bash
			// `&&` chain: echo only on success, entry always consumed).
			continue
		}
		_, _ = fmt.Fprintf(logF, "[cleanup] deferring %s (tip not on origin — no PR yet)\n", row.Branch)
		keep = append(keep, line)
	}
	if len(keep) == 0 {
		_ = os.Remove(d.pendingPath())
		return
	}
	_ = os.WriteFile(d.pendingPath(), []byte(strings.Join(keep, "\n")+"\n"), 0o644)
}
